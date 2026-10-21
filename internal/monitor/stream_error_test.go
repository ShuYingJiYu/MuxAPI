package monitor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/store"
)

func TestMonitorErrorWithDoneCannotRestoreChannel(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded\"}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	manager := health.New(1, time.Minute)
	prober := NewProber(New(st), st, manager, nil, nil)
	prober.Probe(context.Background(), &store.Monitor{ID: 1, UpstreamID: 7, Model: "model", BaseURL: provider.URL, Stream: true})
	states := manager.ModelStates(7)
	if len(states) != 1 || states[0].Kind != "transient" || states[0].LastReason != "overloaded" {
		t.Fatalf("monitor accepted an errored stream: %+v", states)
	}
	if validProbePayload("text/event-stream", []byte("event: error\ndata: {\"message\":\"failed\"}\n\ndata: [DONE]\n\n"), true) {
		t.Fatal("error plus DONE was accepted as a successful probe")
	}
}

func TestMonitorDoesNotSendRequestsDuringExplicitRateLimit(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{"ok":true}`) }))
	defer provider.Close()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	manager := health.New(3, time.Minute)
	lease, _ := manager.Claim(1, 7, "model")
	manager.CompleteModelRateLimited(lease, 5, time.Minute, 429, "limited")
	prober := NewProber(New(st), st, manager, nil, nil)
	prober.Probe(context.Background(), &store.Monitor{ID: 1, UpstreamID: 7, Model: "model", BaseURL: provider.URL})
	if calls.Load() != 0 || manager.IsAvailable(7, "model") {
		t.Fatal("monitor ignored Retry-After or cleared the rate limit")
	}
}
