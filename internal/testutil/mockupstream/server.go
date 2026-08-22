// Package mockupstream provides a configurable mock Anthropic Messages API
// server for end-to-end routing and forwarding tests. It simulates streaming
// SSE responses with cache state tracking per prefix.
package mockupstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// FailureMode controls how the mock responds to requests.
type FailureMode int

const (
	FailureNone           FailureMode = iota
	FailureRateLimit                          // 429
	FailureServerError                        // 500
	FailureModelNotFound                      // 404 with error body
)

// Config configures a mock upstream instance.
type Config struct {
	// SupportedModels lists model IDs this mock will accept. Empty means accept all.
	SupportedModels []string
	// ResponseDelay is added before streaming begins (simulates TTFT).
	ResponseDelay time.Duration
	// CacheTTL is how long a cached prefix stays valid. Zero means no caching.
	CacheTTL time.Duration
	// FailureMode makes the mock return errors.
	Failure FailureMode
	// FailAfterN causes the mock to fail after N successful requests.
	// Zero means never fail (unless Failure is set from the start).
	FailAfterN int
	// OutputText is the text content returned in the response. Default: "Hello."
	OutputText string
	// OutputTokens reported in usage. Default: 5.
	OutputTokens int
}

// RequestRecord stores information about a received request.
type RequestRecord struct {
	Time        time.Time
	Model       string
	InputTokens int
	CacheKey    string
	Headers     http.Header
	Body        []byte
}

// cacheState tracks per-prefix cache entries.
type cacheState struct {
	createdAt time.Time
}

// Server is a mock Anthropic Messages API server.
type Server struct {
	config  Config
	server  *httptest.Server
	mu      sync.Mutex
	history []RequestRecord
	cache   map[string]*cacheState // keyed by prefix hash
	reqCount int
}

// New creates and starts a new mock upstream server.
func New(cfg Config) *Server {
	if cfg.OutputText == "" {
		cfg.OutputText = "Hello."
	}
	if cfg.OutputTokens == 0 {
		cfg.OutputTokens = 5
	}
	s := &Server{
		config:  cfg,
		cache:   make(map[string]*cacheState),
		history: make([]RequestRecord, 0),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	s.server = httptest.NewServer(mux)
	return s
}

// URL returns the base URL of the mock server.
func (s *Server) URL() string {
	return s.server.URL
}

// Close shuts down the server.
func (s *Server) Close() {
	s.server.Close()
}

// History returns a copy of all received requests.
func (s *Server) History() []RequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RequestRecord, len(s.history))
	copy(out, s.history)
	return out
}

// RequestCount returns how many requests have been served.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqCount
}

// ResetHistory clears request history and cache state.
func (s *Server) ResetHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = s.history[:0]
	s.cache = make(map[string]*cacheState)
	s.reqCount = 0
}

// SetFailure changes the failure mode at runtime.
func (s *Server) SetFailure(mode FailureMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.Failure = mode
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		System any  `json:"system"`
		Stream bool `json:"stream"`
	}

	rawBody := make([]byte, 0)
	if r.Body != nil {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				rawBody = append(rawBody, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
	}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid JSON"}}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.reqCount++
	currentCount := s.reqCount
	failure := s.config.Failure
	if s.config.FailAfterN > 0 && currentCount > s.config.FailAfterN && failure == FailureNone {
		failure = FailureServerError
	}

	// Check model support
	modelSupported := true
	if len(s.config.SupportedModels) > 0 {
		modelSupported = false
		for _, m := range s.config.SupportedModels {
			if m == body.Model {
				modelSupported = true
				break
			}
		}
	}

	// Compute cache key from system prompt content
	cacheKey := computeCacheKey(body.System)
	inputTokens := estimateInputTokens(body.Messages, body.System)

	s.history = append(s.history, RequestRecord{
		Time:        time.Now(),
		Model:       body.Model,
		InputTokens: inputTokens,
		CacheKey:    cacheKey,
		Headers:     r.Header.Clone(),
		Body:        rawBody,
	})
	s.mu.Unlock()

	// Apply failure mode
	switch failure {
	case FailureRateLimit:
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "rate_limit_error",
				"message": "Rate limit exceeded",
			},
		})
		return
	case FailureServerError:
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": "Internal server error",
			},
		})
		return
	case FailureModelNotFound:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "not_found_error",
				"message": fmt.Sprintf("model: %s", body.Model),
			},
		})
		return
	}

	if !modelSupported {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("model %q is not supported", body.Model),
			},
		})
		return
	}

	// Simulate response delay
	if s.config.ResponseDelay > 0 {
		time.Sleep(s.config.ResponseDelay)
	}

	// Determine cache behavior
	cacheCreation := 0
	cacheRead := 0
	if s.config.CacheTTL > 0 && cacheKey != "" {
		s.mu.Lock()
		entry, exists := s.cache[cacheKey]
		now := time.Now()
		if exists && now.Sub(entry.createdAt) < s.config.CacheTTL {
			// Cache hit
			cacheRead = inputTokens / 2 // Assume half the input is cacheable prefix
		} else {
			// Cache miss or expired — create new entry
			cacheCreation = inputTokens / 2
			s.cache[cacheKey] = &cacheState{createdAt: now}
		}
		s.mu.Unlock()
	}

	// Stream SSE response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	writeEvent := func(data string) {
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// message_start
	usage := map[string]int{"input_tokens": inputTokens}
	if cacheCreation > 0 {
		usage["cache_creation_input_tokens"] = cacheCreation
	}
	if cacheRead > 0 {
		usage["cache_read_input_tokens"] = cacheRead
	}
	usageJSON, _ := json.Marshal(usage)
	writeEvent(fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_mock_%d","model":"%s","usage":%s}}`,
		currentCount, body.Model, string(usageJSON)))

	// content_block_start
	writeEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)

	// content_block_delta
	writeEvent(fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"%s"}}`,
		s.config.OutputText))

	// content_block_stop
	writeEvent(`{"type":"content_block_stop","index":0}`)

	// message_delta
	writeEvent(fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`,
		s.config.OutputTokens))

	// message_stop
	writeEvent(`{"type":"message_stop"}`)
}

func computeCacheKey(system any) string {
	if system == nil {
		return ""
	}
	data, err := json.Marshal(system)
	if err != nil || string(data) == "null" {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8])
}

func estimateInputTokens(messages []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}, system any) int {
	// Simple estimation: count characters / 4
	total := 0
	if system != nil {
		data, _ := json.Marshal(system)
		total += len(data) / 4
	}
	for _, msg := range messages {
		data, _ := json.Marshal(msg.Content)
		total += len(data) / 4
	}
	if total < 10 {
		total = 10
	}
	return total
}
