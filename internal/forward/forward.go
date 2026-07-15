package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

const defaultFirstResponseTimeout = 120 * time.Second

type Health interface {
	Report(id int64, model string, ok bool, latencyMs int64)
	ReleaseClaim(id int64, model string)
	MarkModelUnsupported(id int64, model string)
	MarkModelSupported(id int64, model string)
}

type Picker interface {
	PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, error)
}

type Forwarder struct {
	picker               Picker
	health               Health
	maxAttempts          int
	firstResponseTimeout func() time.Duration
}

func New(p Picker, h Health, maxRetries int) *Forwarder {
	maxAttempts := maxRetries
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Forwarder{picker: p, health: h, maxAttempts: maxAttempts}
}

func (f *Forwarder) SetFirstResponseTimeout(timeout func() time.Duration) {
	f.firstResponseTimeout = timeout
}

func (f *Forwarder) firstByteTimeout() time.Duration {
	if f.firstResponseTimeout != nil {
		if timeout := f.firstResponseTimeout(); timeout > 0 {
			return timeout
		}
	}
	return defaultFirstResponseTimeout
}

const StatusClientClosedRequest = 499

const (
	OutcomeSuccess     = "success"
	OutcomeFailed      = "failed"
	OutcomeCanceled    = "canceled"
	OutcomePartial     = "partial"
	OutcomeClientError = "client_error"
	OutcomeUnsupported = "unsupported"
	OutcomeUnavailable = "unavailable"
)

type AttemptResult struct {
	AttemptNo   int
	UpstreamID  int64
	Status      int
	Outcome     string
	TTFTMs      int64
	DurationMs  int64
	CreatedAt   time.Time
	CompletedAt time.Time
	Error       string
}

type Result struct {
	Status          int
	Outcome         string
	FinalUpstreamID int64
	TTFTMs          int64
	Error           string
	Attempts        []AttemptResult
}

func attemptResult(number int, upstreamID int64, status int, outcome string, ttft int64, started time.Time, errText string) AttemptResult {
	completed := time.Now()
	return AttemptResult{
		AttemptNo: number, UpstreamID: upstreamID, Status: status, Outcome: outcome,
		TTFTMs: ttft, DurationMs: completed.Sub(started).Milliseconds(),
		CreatedAt: started, CompletedAt: completed, Error: clipErr(errText),
	}
}

func (f *Forwarder) Forward(w http.ResponseWriter, r *http.Request, body []byte, groupID int64, keyName string) Result {
	model := parseModel(body)
	tried := map[int64]bool{}
	var lastErr error
	attempts := make([]AttemptResult, 0, f.maxAttempts)

	for attempt := 0; attempt < f.maxAttempts; attempt++ {
		candidate, err := f.picker.PickExcluding(groupID, model, tried)
		if err != nil {
			break
		}
		tried[candidate.ID] = true
		release := func() { f.health.ReleaseClaim(candidate.ID, model) }
		attemptStarted := time.Now()
		attemptNo := attempt + 1

		req, err := candidate.BuildRequest(r.Method, r.URL.RequestURI(), bytes.NewReader(body), r.Header)
		if err != nil {
			f.health.Report(candidate.ID, model, false, 0)
			release()
			attempts = append(attempts, attemptResult(attemptNo, candidate.ID, 0, OutcomeFailed, 0, attemptStarted, err.Error()))
			lastErr = err
			continue
		}

		ctx, cancel := context.WithCancel(r.Context())
		firstByteTimer := time.AfterFunc(f.firstByteTimeout(), cancel)
		req = req.WithContext(ctx)
		client := &http.Client{Timeout: 0, Transport: candidate.NewTransport()}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			firstByteTimer.Stop()
			cancel()
			release()
			if r.Context().Err() != nil {
				attempts = append(attempts, attemptResult(attemptNo, candidate.ID, StatusClientClosedRequest, OutcomeCanceled, 0, attemptStarted, r.Context().Err().Error()))
				return Result{Status: StatusClientClosedRequest, Outcome: OutcomeCanceled, FinalUpstreamID: candidate.ID, Error: clipErr(r.Context().Err().Error()), Attempts: attempts}
			}
			f.health.Report(candidate.ID, model, false, 0)
			attempts = append(attempts, attemptResult(attemptNo, candidate.ID, 0, OutcomeFailed, 0, attemptStarted, err.Error()))
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
			payload, readErr := readLimitedBody(resp.Body, 2<<20)
			firstByteTimer.Stop()
			cancel()
			resp.Body.Close()
			if readErr != nil {
				release()
				if r.Context().Err() != nil {
					attempts = append(attempts, attemptResult(attemptNo, candidate.ID, StatusClientClosedRequest, OutcomeCanceled, 0, attemptStarted, r.Context().Err().Error()))
					return Result{Status: StatusClientClosedRequest, Outcome: OutcomeCanceled, FinalUpstreamID: candidate.ID, Error: clipErr(r.Context().Err().Error()), Attempts: attempts}
				}
				f.health.Report(candidate.ID, model, false, 0)
				attempts = append(attempts, attemptResult(attemptNo, candidate.ID, 0, OutcomeFailed, 0, attemptStarted, readErr.Error()))
				lastErr = readErr
				continue
			}
			if upstream.IsModelUnsupported(resp.StatusCode, model, string(payload)) {
				f.health.MarkModelUnsupported(candidate.ID, model)
				release()
				attempts = append(attempts, attemptResult(attemptNo, candidate.ID, resp.StatusCode, OutcomeUnsupported,
					time.Since(start).Milliseconds(), attemptStarted, string(payload)))
				continue
			}

			// Other 4xx responses describe the client request. Relay them without
			// changing channel health.
			resp.Body = io.NopCloser(bytes.NewReader(payload))
			result := relayResponse(w, resp, start, nil)
			release()
			if result.err != nil {
				attempts = append(attempts, attemptResult(attemptNo, candidate.ID, StatusClientClosedRequest, OutcomeCanceled,
					result.ttftMs, attemptStarted, result.err.Error()))
				return Result{Status: StatusClientClosedRequest, Outcome: OutcomeCanceled, FinalUpstreamID: candidate.ID,
					TTFTMs: result.ttftMs, Error: clipErr(result.err.Error()), Attempts: attempts}
			}
			attempts = append(attempts, attemptResult(attemptNo, candidate.ID, resp.StatusCode, OutcomeClientError,
				result.ttftMs, attemptStarted, string(payload)))
			return Result{Status: resp.StatusCode, Outcome: OutcomeClientError, FinalUpstreamID: candidate.ID,
				TTFTMs: result.ttftMs, Error: clipErr(string(payload)), Attempts: attempts}
		}

		if upstream.IsFailureStatus(resp.StatusCode) {
			payload, _ := readLimitedBody(resp.Body, 64<<10)
			firstByteTimer.Stop()
			resp.Body.Close()
			cancel()
			latency := time.Since(start).Milliseconds()
			f.health.Report(candidate.ID, model, false, latency)
			release()
			attempts = append(attempts, attemptResult(attemptNo, candidate.ID, resp.StatusCode, OutcomeFailed, latency, attemptStarted, string(payload)))
			lastErr = errors.New(http.StatusText(resp.StatusCode))
			continue
		}

		result := relayResponse(w, resp, start, func() { firstByteTimer.Stop() })
		firstByteTimer.Stop()
		cancel()
		release()

		if result.err != nil {
			if result.source == relayDownstream || r.Context().Err() != nil {
				errText := result.err.Error()
				if r.Context().Err() != nil {
					errText = r.Context().Err().Error()
				}
				attempts = append(attempts, attemptResult(attemptNo, candidate.ID, StatusClientClosedRequest, OutcomeCanceled,
					result.ttftMs, attemptStarted, errText))
				return Result{Status: StatusClientClosedRequest, Outcome: OutcomeCanceled, FinalUpstreamID: candidate.ID,
					TTFTMs: result.ttftMs, Error: clipErr(errText), Attempts: attempts}
			}
			f.health.Report(candidate.ID, model, false, result.ttftMs)
			outcome := OutcomeFailed
			status := 0
			if result.committed {
				outcome = OutcomePartial
				status = resp.StatusCode
			}
			attempts = append(attempts, attemptResult(attemptNo, candidate.ID, status, outcome,
				result.ttftMs, attemptStarted, result.err.Error()))
			lastErr = result.err
			if !result.committed {
				continue
			}
			return Result{Status: resp.StatusCode, Outcome: OutcomePartial, FinalUpstreamID: candidate.ID,
				TTFTMs: result.ttftMs, Error: clipErr(result.err.Error()), Attempts: attempts}
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			f.health.MarkModelSupported(candidate.ID, model)
			f.health.Report(candidate.ID, model, true, result.ttftMs)
		}
		outcome := OutcomeSuccess
		if resp.StatusCode >= 400 {
			outcome = OutcomeClientError
		}
		attempts = append(attempts, attemptResult(attemptNo, candidate.ID, resp.StatusCode, outcome,
			result.ttftMs, attemptStarted, ""))
		return Result{Status: resp.StatusCode, Outcome: outcome, FinalUpstreamID: candidate.ID,
			TTFTMs: result.ttftMs, Attempts: attempts}
	}

	if len(tried) == 0 {
		http.Error(w, "no upstream available", http.StatusServiceUnavailable)
		return Result{Status: http.StatusServiceUnavailable, Outcome: OutcomeUnavailable, Error: "no upstream available", Attempts: attempts}
	}
	if lastErr != nil {
		http.Error(w, "upstream error: "+lastErr.Error(), http.StatusBadGateway)
		return Result{Status: http.StatusBadGateway, Outcome: OutcomeFailed, Error: clipErr(lastErr.Error()), Attempts: attempts}
	}
	http.Error(w, "all upstreams failed", http.StatusBadGateway)
	return Result{Status: http.StatusBadGateway, Outcome: OutcomeFailed, Error: "all upstreams failed", Attempts: attempts}
}

func clipErr(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func readLimitedBody(reader io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, limit))
}

func parseModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

var errEmptyResponse = errors.New("upstream returned an empty response")
var errErrorPayload = errors.New("upstream returned an error payload with a successful status")

type relaySource int

const (
	relayUpstream relaySource = iota
	relayDownstream
)

type relayResult struct {
	err       error
	source    relaySource
	ttftMs    int64
	committed bool
}

func relayResponse(w http.ResponseWriter, resp *http.Response, start time.Time, onFirstByte func()) relayResult {
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		return relaySSE(w, resp, start, onFirstByte)
	}
	return relayBody(w, resp, start, onFirstByte)
}

func relaySSE(w http.ResponseWriter, resp *http.Response, start time.Time, onFirstByte func()) relayResult {
	flusher, canFlush := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	committed := false
	ttft := int64(0)

	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if !committed {
				ttft = time.Since(start).Milliseconds()
				if onFirstByte != nil {
					onFirstByte()
				}
				copyResponseHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				committed = true
			}
			if _, err := w.Write(buffer[:n]); err != nil {
				return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true}
			}
			if canFlush {
				flusher.Flush()
			}
		}

		if readErr != nil {
			if readErr == io.EOF && committed {
				return relayResult{ttftMs: ttft, committed: true}
			}
			if readErr == io.EOF {
				readErr = errEmptyResponse
			}
			return relayResult{err: readErr, source: relayUpstream, ttftMs: ttft, committed: committed}
		}
	}
}

func relayBody(w http.ResponseWriter, resp *http.Response, start time.Time, onFirstByte func()) relayResult {
	const inspectLimit = 64 << 10
	buffer := make([]byte, 32*1024)
	ttft := int64(0)
	firstByteSeen := false
	var pending bytes.Buffer
	for pending.Len() <= inspectLimit {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if !firstByteSeen {
				firstByteSeen = true
				ttft = time.Since(start).Milliseconds()
				if onFirstByte != nil {
					onFirstByte()
				}
			}
			pending.Write(buffer[:n])
		}
		if readErr != nil {
			if readErr != io.EOF {
				return relayResult{err: readErr, source: relayUpstream, ttftMs: ttft}
			}
			if pending.Len() == 0 {
				if resp.StatusCode == http.StatusNoContent {
					copyResponseHeaders(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					return relayResult{ttftMs: time.Since(start).Milliseconds(), committed: true}
				}
				return relayResult{err: errEmptyResponse, source: relayUpstream}
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && upstream.IsErrorPayload(pending.Bytes()) {
				return relayResult{err: errErrorPayload, source: relayUpstream, ttftMs: ttft}
			}
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if _, err := w.Write(pending.Bytes()); err != nil {
				return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true}
			}
			return relayResult{ttftMs: ttft, committed: true}
		}
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(pending.Bytes()); err != nil {
		return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true}
	}
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return relayResult{err: err, source: relayDownstream, ttftMs: ttft, committed: true}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return relayResult{ttftMs: ttft, committed: true}
			}
			return relayResult{err: readErr, source: relayUpstream, ttftMs: ttft, committed: true}
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}
