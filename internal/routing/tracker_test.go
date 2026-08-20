package routing

import (
	"sync"
	"testing"
	"time"
)

func TestTrackerWindowStatsAndForecast(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := NewTracker(10*time.Minute, 100)
	tracker.Observe(Observation{At: now.Add(-20 * time.Minute), UpstreamID: 1, Model: "m", CacheKey: "k", InputTokens: 999, Success: true})
	tracker.Observe(Observation{At: now.Add(-2 * time.Minute), UpstreamID: 1, Model: "m", Protocol: "claude", CacheKey: "k", InputTokens: 1_000, OutputTokens: 100, CacheCreationTokens: 800, TTFTMs: 100, DurationMs: 1_000, Success: true})
	tracker.Observe(Observation{At: now.Add(-time.Minute), UpstreamID: 1, Model: "m", Protocol: "claude", CacheKey: "k", InputTokens: 200, OutputTokens: 300, CachedTokens: 800, TTFTMs: 200, DurationMs: 2_000, Success: false})
	tracker.Observe(Observation{At: now.Add(-time.Minute), UpstreamID: 2, Model: "m", CacheKey: "k", InputTokens: 5_000, Success: true})

	stats := tracker.Snapshot(now, 1, "m", "k")
	if stats.Requests != 2 || stats.Successes != 1 || stats.InputTokens != 1_200 || stats.OutputTokens != 400 {
		t.Fatalf("unexpected aggregate: %+v", stats)
	}
	closeTo(t, stats.RequestsPerMinute, 0.2)
	closeTo(t, stats.OutputPerRequest, 200)
	closeTo(t, stats.HitRate, 0.5)
	closeTo(t, stats.SuccessRate, 0.5)
	closeTo(t, stats.P50TTFTMs, 100)
	closeTo(t, stats.P95TTFTMs, 100)
	closeTo(t, stats.P95DurationMs, 1_000)

	forecast := tracker.Forecast(now, 5*time.Minute, 1, "m", "k")
	closeTo(t, forecast.RequestsPerMinute, 0.2)
	closeTo(t, forecast.OutputTokensPerReq, 200)
	if forecast.Window != 5*time.Minute {
		t.Fatalf("forecast window = %s", forecast.Window)
	}
}

func TestTrackerIsConcurrentAndBounded(t *testing.T) {
	now := time.Now()
	tracker := NewTracker(time.Hour, 20)
	var wait sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func(id int64) {
			defer wait.Done()
			for i := 0; i < 25; i++ {
				tracker.Observe(Observation{At: now, UpstreamID: id, Model: "m", InputTokens: 1, Success: true})
				_ = tracker.Snapshot(now, 0, "m", "")
			}
		}(int64(worker + 1))
	}
	wait.Wait()
	stats := tracker.Snapshot(now, 0, "m", "")
	if stats.Requests != 20 {
		t.Fatalf("bounded tracker retained %d observations, want 20", stats.Requests)
	}
}
