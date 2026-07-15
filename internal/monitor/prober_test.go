package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/store"
)

type breakerCall struct {
	id    int64
	model string
	ok    bool
	lat   int64
}

type fakeBreaker struct {
	calls       []breakerCall
	unsupported []string
	supported   []string
}

func (f *fakeBreaker) ObserveProbe(id int64, model string, ok bool, latencyMs int64) {
	f.calls = append(f.calls, breakerCall{id: id, model: model, ok: ok, lat: latencyMs})
}
func (f *fakeBreaker) MarkModelUnsupported(id int64, model string) {
	f.unsupported = append(f.unsupported, model)
}
func (f *fakeBreaker) MarkModelSupported(id int64, model string) {
	f.supported = append(f.supported, model)
}

func TestBuildProbeBodyChat(t *testing.T) {
	m := &store.Monitor{Model: "gpt-x", ProbeText: `say "ok"`, MaxTokens: 5, Stream: true}
	var payload map[string]any
	if err := json.Unmarshal(buildProbeBody(m), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "gpt-x" || payload["max_tokens"].(float64) != 5 || payload["stream"] != true {
		t.Fatalf("unexpected chat payload: %+v", payload)
	}
	if payload["messages"].([]any)[0].(map[string]any)["content"] != `say "ok"` {
		t.Fatalf("probe text was not preserved: %+v", payload)
	}
}

func TestBuildProbeBodyResponses(t *testing.T) {
	m := &store.Monitor{Model: "gpt-x", Path: "/v1/responses", ProbeText: "hi", MaxTokens: 2}
	var payload map[string]any
	if err := json.Unmarshal(buildProbeBody(m), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["input"] != "hi" || payload["max_output_tokens"].(float64) != 2 {
		t.Fatalf("unexpected responses payload: %+v", payload)
	}
	if _, exists := payload["messages"]; exists {
		t.Fatalf("responses payload must not contain chat messages: %+v", payload)
	}
}

func TestValidProbePayload(t *testing.T) {
	cases := []struct {
		name   string
		ct     string
		body   string
		stream bool
		want   bool
	}{
		{"json", "application/json", `{"ok":true}`, false, true},
		{"empty", "application/json", ``, false, false},
		{"invalid-json", "application/json", `ok`, false, false},
		{"json-error", "application/json", `{"error":{"message":"failed"}}`, false, false},
		{"sse-complete", "text/event-stream", "data: {}\ndata: [DONE]\n", true, true},
		{"sse-incomplete", "text/event-stream", "data: {}\n", true, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := validProbePayload(test.ct, []byte(test.body), test.stream); got != test.want {
				t.Fatalf("got %v want %v", got, test.want)
			}
		})
	}
}

func TestObserveAlwaysUsesChannelBreaker(t *testing.T) {
	fake := &fakeBreaker{}
	prober := &Prober{breaker: fake}
	monitor := &store.Monitor{UpstreamID: 7, Model: "gpt-x", ChannelProbe: false}
	prober.observe(monitor, false, 0, 0)
	prober.observe(monitor, true, 429, 50)
	prober.observe(monitor, true, 200, 40)
	if len(fake.calls) != 3 {
		t.Fatalf("expected three breaker calls, got %+v", fake.calls)
	}
	for _, call := range fake.calls {
		if call.id != 7 || call.model != "gpt-x" {
			t.Fatalf("unexpected channel call: %+v", call)
		}
	}
	if fake.calls[0].ok || fake.calls[1].ok || !fake.calls[2].ok {
		t.Fatalf("unexpected outcomes: %+v", fake.calls)
	}
}

func TestProbeRecoveryNeedsTwoSuccesses(t *testing.T) {
	mgr := health.New(1, time.Hour)
	prober := &Prober{breaker: mgr}
	monitor := &store.Monitor{UpstreamID: 9, Model: "gpt-x"}
	prober.observe(monitor, false, 0, 0)
	if got := mgr.EffectiveState(9); got != "OPEN" {
		t.Fatalf("expected OPEN, got %s", got)
	}
	prober.observe(monitor, true, 200, 30)
	if got := mgr.EffectiveState(9); got != "HALF_OPEN" {
		t.Fatalf("first success should enter HALF_OPEN, got %s", got)
	}
	prober.observe(monitor, true, 200, 25)
	if got := mgr.EffectiveState(9); got != "CLOSED" {
		t.Fatalf("second success should close channel, got %s", got)
	}
}

func TestProbeRejectsIncompleteStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"chunk\":1}\n")
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hm := health.New(1, time.Hour)
	prober := NewProber(New(st), st, hm, nil, nil)
	item := &store.Monitor{
		ID: 1, UpstreamID: 7, Model: "gpt", BaseURL: server.URL,
		APIKey: "k", Path: "/v1/chat/completions", Stream: true,
	}
	prober.Probe(context.Background(), item)
	if got := hm.EffectiveState(7); got != "OPEN" {
		t.Fatalf("incomplete 200 stream should count as channel failure, got %s", got)
	}
	if got := prober.mgr.Snapshot(1).State; got != "DOWN" {
		t.Fatalf("monitor should show DOWN, got %s", got)
	}
}

func TestEffIntervalLowerBound(t *testing.T) {
	const globalInterval = 5 * time.Minute
	prober := NewProber(nil, nil, nil, func() time.Duration { return globalInterval }, nil)
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		{1, minIntervalSec * time.Second},
		{minIntervalSec, minIntervalSec * time.Second},
		{120, 120 * time.Second},
		{0, globalInterval},
	}
	for _, test := range cases {
		monitor := &store.Monitor{IntervalSec: test.seconds}
		if got := prober.effInterval(monitor); got != test.want {
			t.Fatalf("seconds=%d got=%v want=%v", test.seconds, got, test.want)
		}
	}
}
