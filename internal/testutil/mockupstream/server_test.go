package mockupstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMockServerBasicStream(t *testing.T) {
	srv := New(Config{OutputText: "Hello world", OutputTokens: 3})
	defer srv.Close()

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(srv.URL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	events := parseSSEEvents(t, resp.Body)
	if len(events) < 6 {
		t.Fatalf("got %d events, want at least 6", len(events))
	}

	// Verify message_start
	var msgStart struct {
		Type    string `json:"type"`
		Message struct {
			Model string         `json:"model"`
			Usage map[string]int `json:"usage"`
		} `json:"message"`
	}
	json.Unmarshal([]byte(events[0]), &msgStart)
	if msgStart.Type != "message_start" {
		t.Fatalf("first event type = %q", msgStart.Type)
	}
	if msgStart.Message.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("model = %q", msgStart.Message.Model)
	}

	// Verify content includes our text
	var delta struct {
		Type  string `json:"type"`
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	json.Unmarshal([]byte(events[2]), &delta)
	if delta.Delta.Text != "Hello world" {
		t.Fatalf("delta text = %q", delta.Delta.Text)
	}

	// Verify message_stop is last
	var stop struct{ Type string }
	json.Unmarshal([]byte(events[len(events)-1]), &stop)
	if stop.Type != "message_stop" {
		t.Fatalf("last event type = %q", stop.Type)
	}

	// Request recorded
	if srv.RequestCount() != 1 {
		t.Fatalf("request count = %d", srv.RequestCount())
	}
	history := srv.History()
	if history[0].Model != "claude-sonnet-4-20250514" {
		t.Fatalf("history model = %q", history[0].Model)
	}
}

func TestMockServerCacheBehavior(t *testing.T) {
	srv := New(Config{CacheTTL: 100 * time.Millisecond})
	defer srv.Close()

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"system":"You are a helpful assistant","stream":true}`

	// First request: should get cache_creation
	resp1 := mustPost(t, srv.URL()+"/v1/messages", body)
	events1 := parseSSEEvents(t, resp1.Body)
	resp1.Body.Close()
	usage1 := extractUsage(t, events1[0])
	if _, ok := usage1["cache_creation_input_tokens"]; !ok {
		t.Fatalf("first request should have cache_creation_input_tokens: %v", usage1)
	}
	if _, ok := usage1["cache_read_input_tokens"]; ok {
		t.Fatalf("first request should not have cache_read_input_tokens: %v", usage1)
	}

	// Second request (within TTL): should get cache_read
	resp2 := mustPost(t, srv.URL()+"/v1/messages", body)
	events2 := parseSSEEvents(t, resp2.Body)
	resp2.Body.Close()
	usage2 := extractUsage(t, events2[0])
	if _, ok := usage2["cache_read_input_tokens"]; !ok {
		t.Fatalf("second request should have cache_read_input_tokens: %v", usage2)
	}
	if _, ok := usage2["cache_creation_input_tokens"]; ok {
		t.Fatalf("second request should not have cache_creation_input_tokens: %v", usage2)
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Third request (after TTL): should get cache_creation again
	resp3 := mustPost(t, srv.URL()+"/v1/messages", body)
	events3 := parseSSEEvents(t, resp3.Body)
	resp3.Body.Close()
	usage3 := extractUsage(t, events3[0])
	if _, ok := usage3["cache_creation_input_tokens"]; !ok {
		t.Fatalf("post-TTL request should have cache_creation_input_tokens: %v", usage3)
	}
}

func TestMockServerFailureModes(t *testing.T) {
	t.Run("rate_limit", func(t *testing.T) {
		srv := New(Config{Failure: FailureRateLimit})
		defer srv.Close()
		resp := mustPost(t, srv.URL()+"/v1/messages", `{"model":"x","messages":[]}`)
		defer resp.Body.Close()
		if resp.StatusCode != 429 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("server_error", func(t *testing.T) {
		srv := New(Config{Failure: FailureServerError})
		defer srv.Close()
		resp := mustPost(t, srv.URL()+"/v1/messages", `{"model":"x","messages":[]}`)
		defer resp.Body.Close()
		if resp.StatusCode != 500 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("model_not_found", func(t *testing.T) {
		srv := New(Config{Failure: FailureModelNotFound})
		defer srv.Close()
		resp := mustPost(t, srv.URL()+"/v1/messages", `{"model":"x","messages":[]}`)
		defer resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("unsupported_model", func(t *testing.T) {
		srv := New(Config{SupportedModels: []string{"claude-sonnet-4-20250514"}})
		defer srv.Close()
		resp := mustPost(t, srv.URL()+"/v1/messages", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
		defer resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("status = %d, want 400 for unsupported model", resp.StatusCode)
		}
	})
}

func TestMockServerFailAfterN(t *testing.T) {
	srv := New(Config{FailAfterN: 2})
	defer srv.Close()

	body := `{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`

	// First two succeed
	for i := 0; i < 2; i++ {
		resp := mustPost(t, srv.URL()+"/v1/messages", body)
		if resp.StatusCode != 200 {
			t.Fatalf("request %d status = %d", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Third fails
	resp := mustPost(t, srv.URL()+"/v1/messages", body)
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("request 3 status = %d, want 500", resp.StatusCode)
	}
}

func TestMockServerSetFailureRuntime(t *testing.T) {
	srv := New(Config{})
	defer srv.Close()

	body := `{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp := mustPost(t, srv.URL()+"/v1/messages", body)
	if resp.StatusCode != 200 {
		t.Fatalf("initial request failed: %d", resp.StatusCode)
	}
	resp.Body.Close()

	srv.SetFailure(FailureRateLimit)
	resp = mustPost(t, srv.URL()+"/v1/messages", body)
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("after SetFailure: status = %d, want 429", resp.StatusCode)
	}
}

// --- helpers ---

func mustPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func parseSSEEvents(t *testing.T, r io.Reader) []string {
	t.Helper()
	var events []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			events = append(events, data)
		}
	}
	return events
}

func extractUsage(t *testing.T, messageStartData string) map[string]int {
	t.Helper()
	var raw struct {
		Message struct {
			Usage map[string]int `json:"usage"`
		} `json:"message"`
	}
	if err := json.NewDecoder(bytes.NewReader([]byte(messageStartData))).Decode(&raw); err != nil {
		t.Fatalf("parse message_start: %v", err)
	}
	return raw.Message.Usage
}
