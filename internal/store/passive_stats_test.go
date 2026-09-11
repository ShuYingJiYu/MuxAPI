package store

import (
	"context"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestPassiveStatsUsesRequestAndAttemptSemantics(t *testing.T) {
	st, err := Open(t.TempDir() + "/passive.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Create(&upstream.Upstream{ID: 7, Name: "provider", BaseURL: "http://provider", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	if ok := st.EnqueueRequest(RequestRecord{
		RequestID: "passive-retry", FinalUpstreamID: 7, Model: "client-model", Status: 200,
		Outcome: forward.OutcomeSuccess, TTFTMs: 100, DurationMs: 300,
		CreatedAt: now.Add(-10 * time.Minute), CompletedAt: now.Add(-9 * time.Minute),
		Attempts: []RequestAttemptRecord{
			{AttemptNo: 1, UpstreamID: 7, MappedModel: "", Status: 503, Outcome: forward.OutcomeFailed, DurationMs: 50, CreatedAt: now.Add(-10 * time.Minute), CompletedAt: now.Add(-10 * time.Minute)},
			{AttemptNo: 2, UpstreamID: 7, MappedModel: "provider-model", Status: 200, Outcome: forward.OutcomeSuccess, TTFTMs: 100, DurationMs: 200, CreatedAt: now.Add(-10 * time.Minute), CompletedAt: now.Add(-9 * time.Minute)},
		},
	}); !ok {
		t.Fatal("enqueue retry request failed")
	}
	if ok := st.EnqueueRequest(RequestRecord{
		RequestID: "passive-neutral", FinalUpstreamID: 7, Model: "client-model", Status: 499,
		Outcome: forward.OutcomeCanceled, CreatedAt: now.Add(-5 * time.Minute), CompletedAt: now.Add(-5 * time.Minute),
		Attempts: []RequestAttemptRecord{{AttemptNo: 1, UpstreamID: 7, MappedModel: "provider-model", Status: 499, Outcome: forward.OutcomeCanceled, CreatedAt: now.Add(-5 * time.Minute), CompletedAt: now.Add(-5 * time.Minute)}},
	}); !ok {
		t.Fatal("enqueue neutral request failed")
	}
	// The second attempt finishes after the window boundary; history membership
	// follows the parent request timestamp, not an individual attempt timestamp.
	if ok := st.EnqueueRequest(RequestRecord{
		RequestID: "passive-boundary", FinalUpstreamID: 7, Model: "boundary-model", Status: 200,
		Outcome: forward.OutcomeSuccess, CreatedAt: now.Add(-2 * time.Hour), CompletedAt: now.Add(-2 * time.Hour),
		Attempts: []RequestAttemptRecord{{AttemptNo: 1, UpstreamID: 7, MappedModel: "boundary-model", Status: 200, Outcome: forward.OutcomeSuccess, CreatedAt: now.Add(-30 * time.Minute), CompletedAt: now.Add(-30 * time.Minute)}},
	}); !ok {
		t.Fatal("enqueue boundary request failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := st.FlushRequests(ctx); err != nil {
		t.Fatal(err)
	}
	window, err := st.PassiveStats(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if window.Requests.Total != 2 || window.Requests.Retried != 1 {
		t.Fatalf("request window = %+v", window.Requests)
	}
	if window.RequestHealth.Eligible != 1 || window.RequestHealth.Successes != 1 ||
		window.RequestHealth.Failures != 0 || window.RequestHealth.Neutral != 1 ||
		window.RequestHealth.SuccessRate != 1 {
		t.Fatalf("request health = %+v", window.RequestHealth)
	}
	if len(window.Channels) != 2 {
		t.Fatalf("channels = %+v", window.Channels)
	}
	var providerModel, fallbackModel PassiveChannelStats
	for _, channel := range window.Channels {
		switch channel.Model {
		case "provider-model":
			providerModel = channel
		case "client-model":
			fallbackModel = channel
		}
	}
	if providerModel.Attempts != 2 || providerModel.Successes != 1 || providerModel.Failures != 0 || providerModel.Neutral != 1 || providerModel.SuccessRate != 1 {
		t.Fatalf("provider model stats = %+v", providerModel)
	}
	if fallbackModel.Attempts != 1 || fallbackModel.Neutral != 0 || fallbackModel.Failures != 1 || fallbackModel.SuccessRate != 0 {
		t.Fatalf("fallback model stats = %+v", fallbackModel)
	}
	if window.Since != now.Add(-time.Hour).Unix() {
		t.Fatalf("since = %d, want %d", window.Since, now.Add(-time.Hour).Unix())
	}
}

func TestPassiveStatsEmptyWindow(t *testing.T) {
	st, err := Open(t.TempDir() + "/passive-empty.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	window, err := st.PassiveStats(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if window.Requests.Total != 0 || len(window.Channels) != 0 {
		t.Fatalf("empty window = %+v", window)
	}
}
