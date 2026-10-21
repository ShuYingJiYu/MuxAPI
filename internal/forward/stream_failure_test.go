package forward

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestFirstStreamErrorFailsOverBeforeOutputAcrossProtocols(t *testing.T) {
	for _, test := range []struct{ name, protocol, payload string }{
		{"responses", "passthrough", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"overloaded\"}}}\n\n"},
		{"openai", "passthrough", "data: {\"error\":{\"message\":\"overloaded\"}}\n\ndata: [DONE]\n\n"},
		{"translated claude", "claude", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n"},
		{"multiline", "passthrough", "event: error\ndata: {\n" + "data: \"error\":{\"message\":\"overloaded\"}\n" + "data: }\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, test.payload)
			}))
			defer primary.Close()
			var calls atomic.Int32
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"recovered\",\"output\":[]}}\n\n")
			}))
			defer backup.Close()
			channels := []*upstream.Upstream{
				{ID: 1, BaseURL: primary.URL, Protocol: test.protocol, Priority: 1},
				{ID: 2, BaseURL: backup.URL, Protocol: "passthrough", Priority: 2},
			}
			manager := health.New(1, 10*time.Minute)
			fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return channels }, manager), manager, 2)
			body := []byte(`{"model":"test-model","input":"hello","stream":true}`)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			request.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"test"}`)
			recorder := httptest.NewRecorder()
			result := fwd.Forward(recorder, request, body, 1, "")
			if result.Outcome != OutcomeSuccess || calls.Load() != 1 || len(result.Attempts) != 2 || strings.Contains(recorder.Body.String(), "overloaded") {
				t.Fatalf("first error prevented clean failover: result=%+v calls=%d body=%s", result, calls.Load(), recorder.Body.String())
			}
			if result.Attempts[0].Error != "overloaded" || manager.EffectiveState(1) != "CLOSED" {
				t.Fatalf("stream error classification or reason lost: %+v", result.Attempts[0])
			}
		})
	}
}

func TestStreamParameterErrorDoesNotRetryOrCoolChannel(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"input too long\"}}}\n\n")
	}))
	defer provider.Close()
	channels := []*upstream.Upstream{{ID: 1, BaseURL: provider.URL, Protocol: "passthrough", Priority: 1}, {ID: 2, BaseURL: provider.URL, Protocol: "passthrough", Priority: 2}}
	manager := health.New(1, time.Minute)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return channels }, manager), manager, 2)
	body := []byte(`{"model":"test-model","stream":true}`)
	recorder := httptest.NewRecorder()
	result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")
	if result.Outcome != OutcomeClientError || recorder.Code != 400 || len(result.Attempts) != 1 || len(manager.ModelStates(1)) != 0 || manager.Snapshot(1).Fails != 0 {
		t.Fatalf("client error affected health or retried: %+v", result)
	}
}

type releaseOnOutputWriter struct {
	*httptest.ResponseRecorder
	once    sync.Once
	release chan struct{}
}

func (w *releaseOnOutputWriter) Write(payload []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(payload)
	w.once.Do(func() { close(w.release) })
	return n, err
}

func TestErrorAfterCommittedOutputDoesNotSwitchUpstreams(t *testing.T) {
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"error\":{\"message\":\"late failure\"}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	channels := []*upstream.Upstream{{ID: 1, BaseURL: provider.URL, Protocol: "passthrough", Priority: 1}, {ID: 2, BaseURL: provider.URL, Protocol: "passthrough", Priority: 2}}
	manager := health.New(1, time.Minute)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return channels }, manager), manager, 2)
	body := []byte(`{"model":"test-model","stream":true}`)
	writer := &releaseOnOutputWriter{ResponseRecorder: httptest.NewRecorder(), release: release}
	result := fwd.Forward(writer, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")
	if result.Outcome != OutcomePartial || len(result.Attempts) != 1 || result.Error != "late failure" || result.StreamCompleted {
		t.Fatalf("committed stream failed over or lost its failure: %+v", result)
	}
}
