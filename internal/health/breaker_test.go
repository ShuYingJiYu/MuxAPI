package health

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelFailuresAccumulateAcrossModels(t *testing.T) {
	m := New(3, time.Hour)
	const id = int64(1)
	m.Report(id, "gpt-5.6", false, 0)
	m.Report(id, "gpt-5.5", false, 0)
	if !m.IsAvailable(id, "gpt-5.4") {
		t.Fatal("two failures should remain below threshold")
	}
	m.Report(id, "claude-opus", false, 0)
	if m.IsAvailable(id, "gpt-5.4") {
		t.Fatal("failures from different models must open the channel")
	}
	if got := m.EffectiveState(id); got != "OPEN" {
		t.Fatalf("expected OPEN, got %s", got)
	}
}

func TestClosedSuccessResetsConsecutiveFailures(t *testing.T) {
	m := New(3, time.Hour)
	m.Report(1, "a", false, 0)
	m.Report(1, "b", false, 0)
	m.Report(1, "c", true, 40)
	m.Report(1, "d", false, 0)
	if !m.IsAvailable(1, "e") {
		t.Fatal("a successful channel request should reset consecutive failures")
	}
}

func TestRecoveryRequiresTwoSuccesses(t *testing.T) {
	m := New(1, time.Hour)
	m.Report(1, "gpt", false, 0)
	m.ObserveProbe(1, "gpt", true, 50)
	if got := m.EffectiveState(1); got != "HALF_OPEN" {
		t.Fatalf("first recovery success should enter HALF_OPEN, got %s", got)
	}
	m.ObserveProbe(1, "gpt", true, 45)
	if got := m.EffectiveState(1); got != "CLOSED" {
		t.Fatalf("second recovery success should close the channel, got %s", got)
	}
}

func TestRecoveryFailureReopens(t *testing.T) {
	m := New(1, 10*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	m.ObserveProbe(1, "gpt", true, 50)
	m.ObserveProbe(1, "gpt", false, 0)
	if got := m.EffectiveState(1); got != "OPEN" {
		t.Fatalf("HALF_OPEN failure should reopen the channel, got %s", got)
	}
}

func TestHalfOpenAllowsOneClaim(t *testing.T) {
	m := New(1, 10*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	time.Sleep(15 * time.Millisecond)
	if !m.IsAvailable(1, "gpt") {
		t.Fatal("cooldown expiry should expose one HALF_OPEN slot")
	}
	if !m.Claim(1, "gpt") {
		t.Fatal("first HALF_OPEN claim should pass")
	}
	if m.Claim(1, "gpt") {
		t.Fatal("second concurrent HALF_OPEN claim should be blocked")
	}
	m.ReleaseClaim(1, "gpt")
	if !m.Claim(1, "gpt") {
		t.Fatal("released HALF_OPEN slot should be reusable")
	}
	m.ReleaseClaim(1, "gpt")
}

func TestHalfOpenConcurrentBurst(t *testing.T) {
	m := New(1, 5*time.Millisecond)
	m.Report(1, "gpt", false, 0)
	time.Sleep(10 * time.Millisecond)
	var accepted int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.Claim(1, "gpt") {
				atomic.AddInt32(&accepted, 1)
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("expected one HALF_OPEN claim, got %d", accepted)
	}
	m.ReleaseClaim(1, "gpt")
}

func TestModelUnsupportedDoesNotAffectChannel(t *testing.T) {
	m := New(1, time.Hour)
	m.MarkModelUnsupported(1, "gpt-5.6")
	if m.IsAvailable(1, "gpt-5.6") {
		t.Fatal("unsupported model should be excluded")
	}
	if !m.IsAvailable(1, "gpt-5.5") {
		t.Fatal("model capability must not affect channel health")
	}
	if got := m.EffectiveState(1); got != "CLOSED" {
		t.Fatalf("capability exclusion must not open channel, got %s", got)
	}
	states := m.ModelStates(1)
	if len(states) != 1 || states[0].Model != "gpt-5.6" || states[0].State != "UNSUPPORTED" {
		t.Fatalf("unexpected capability states: %+v", states)
	}
	m.MarkModelSupported(1, "gpt-5.6")
	if !m.IsAvailable(1, "gpt-5.6") {
		t.Fatal("supported model should be eligible again")
	}
}

func TestModelUnsupportedExpires(t *testing.T) {
	m := New(1, time.Hour)
	key := modelKey{1, "gpt"}
	m.unsupported[key] = time.Now().Add(-time.Second)
	if !m.IsAvailable(1, "gpt") {
		t.Fatal("expired capability exclusion should be removed")
	}
}

func TestInFlightAccounting(t *testing.T) {
	m := New(3, time.Hour)
	if !m.Claim(1, "gpt") || !m.Claim(1, "gpt") {
		t.Fatal("closed channel claims should pass")
	}
	if got := m.InFlight(1); got != 2 {
		t.Fatalf("expected in-flight=2, got %d", got)
	}
	m.ReleaseClaim(1, "gpt")
	m.ReleaseClaim(1, "gpt")
	m.ReleaseClaim(1, "gpt")
	if got := m.InFlight(1); got != 0 {
		t.Fatalf("release must be idempotent at zero, got %d", got)
	}
}

func TestRouteStatsUseChannelTTFT(t *testing.T) {
	m := New(3, time.Hour)
	m.Report(1, "gpt-5.6", true, 100)
	m.Report(1, "gpt-5.5", true, 200)
	latA, succA := m.RouteStats(1, "gpt-5.6")
	latB, succB := m.RouteStats(1, "other")
	if latA != latB || succA != succB {
		t.Fatalf("all models must share channel stats: a=(%v,%v) b=(%v,%v)", latA, succA, latB, succB)
	}
	if latA <= 100 || latA >= 200 || succA != 1 {
		t.Fatalf("unexpected channel EWMA: latency=%v success=%v", latA, succA)
	}
}

func TestTrafficStats(t *testing.T) {
	m := New(3, time.Hour)
	m.Report(1, "a", true, 100)
	m.Report(1, "b", false, 0)
	m.Report(1, "c", true, 300)
	snapshot := m.Snapshot(1)
	if snapshot.Reqs != 3 || snapshot.FailReqs != 1 {
		t.Fatalf("unexpected counters: %+v", snapshot)
	}
	if snapshot.SuccRate < 0.66 || snapshot.SuccRate > 0.67 {
		t.Fatalf("unexpected success rate: %v", snapshot.SuccRate)
	}
	if snapshot.AvgLatMs != 200 {
		t.Fatalf("unexpected average TTFT: %d", snapshot.AvgLatMs)
	}
}

func TestSeedRestoresChannelStatsOnly(t *testing.T) {
	m := New(3, time.Hour)
	m.Seed([]RouteSample{
		{UpstreamID: 1, Model: "a", OK: true, LatencyMs: 100},
		{UpstreamID: 1, Model: "b", OK: false},
		{UpstreamID: 1, Model: "c", OK: true, LatencyMs: 200},
	})
	latency, success := m.RouteStats(1, "any")
	if latency <= 0 || success <= 0 || success >= 1 {
		t.Fatalf("unexpected restored stats: latency=%v success=%v", latency, success)
	}
	if got := m.EffectiveState(1); got != "CLOSED" {
		t.Fatalf("seed must not restore OPEN state, got %s", got)
	}
}

func TestSampleTrend(t *testing.T) {
	m := New(1, time.Hour)
	m.Report(1, "gpt", false, 0)
	m.Sample()
	snapshot := m.Snapshot(1)
	if len(snapshot.Trend) != 1 || snapshot.Trend[0].Status != statDown {
		t.Fatalf("unexpected trend: %+v", snapshot.Trend)
	}
}
