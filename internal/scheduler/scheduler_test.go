package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestStrictPriorityAndFailback(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "primary", Priority: 10, Weight: 1},
		{ID: 2, Name: "backup", Priority: 20, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)

	chosen, _ := scheduler.Pick(1, "gpt")
	if chosen.ID != 1 {
		t.Fatalf("expected primary, got %s", chosen.Name)
	}
	hm.ReleaseClaim(chosen.ID, "gpt")
	hm.Report(1, "gpt", false, 0)

	chosen, _ = scheduler.Pick(1, "gpt")
	if chosen.ID != 2 {
		t.Fatalf("expected backup while primary is open, got %s", chosen.Name)
	}
	hm.ReleaseClaim(chosen.ID, "gpt")

	hm.ObserveProbe(1, "gpt", true, 50)
	hm.ObserveProbe(1, "gpt", true, 45)
	chosen, _ = scheduler.Pick(1, "gpt")
	if chosen.ID != 1 {
		t.Fatalf("expected immediate failback to primary, got %s", chosen.Name)
	}
	hm.ReleaseClaim(chosen.ID, "gpt")
}

func TestFailuresAcrossModelsOpenChannel(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1},
		{ID: 2, Name: "B", Priority: 2, Weight: 1},
	}
	hm := health.New(2, time.Hour)
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)
	hm.Report(1, "gpt-5.6", false, 0)
	hm.Report(1, "gpt-5.5", false, 0)
	chosen, _ := scheduler.Pick(1, "claude")
	if chosen.ID != 2 {
		t.Fatalf("cross-model failures should open A and select B, got %s", chosen.Name)
	}
	hm.ReleaseClaim(chosen.ID, "claude")
}

func TestUnsupportedModelOnlyExcludesThatModel(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1},
		{ID: 2, Name: "B", Priority: 2, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	hm.MarkModelUnsupported(1, "gpt-5.6")
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)

	chosen, _ := scheduler.Pick(1, "gpt-5.6")
	if chosen.ID != 2 {
		t.Fatalf("unsupported model should use B, got %s", chosen.Name)
	}
	hm.ReleaseClaim(chosen.ID, "gpt-5.6")
	chosen, _ = scheduler.Pick(1, "gpt-5.5")
	if chosen.ID != 1 {
		t.Fatalf("other models should still use A, got %s", chosen.Name)
	}
	hm.ReleaseClaim(chosen.ID, "gpt-5.5")
}

func TestSamePriorityWeightDistribution(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 3},
		{ID: 2, Name: "B", Priority: 1, Weight: 1},
	}
	hm := health.New(10, time.Hour)
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)
	counts := map[int64]int{}
	for i := 0; i < 10000; i++ {
		chosen, err := scheduler.Pick(1, "gpt")
		if err != nil {
			t.Fatal(err)
		}
		counts[chosen.ID]++
		hm.ReleaseClaim(chosen.ID, "gpt")
	}
	ratio := float64(counts[1]) / 10000
	if ratio < 0.72 || ratio > 0.78 {
		t.Fatalf("equal-score weight 3:1 should remain near 75%%, got %.3f", ratio)
	}
}

func TestStandardP2CFastCandidateGetsAboutSeventyFivePercent(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "fast", Priority: 1, Weight: 1},
		{ID: 2, Name: "slow", Priority: 1, Weight: 1},
	}
	hm := health.New(10, time.Hour)
	for i := 0; i < 10; i++ {
		hm.Report(1, "gpt", true, 50)
		hm.Report(2, "gpt", true, 500)
	}
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)
	counts := map[int64]int{}
	for i := 0; i < 10000; i++ {
		chosen, _ := scheduler.Pick(1, "gpt")
		counts[chosen.ID]++
		hm.ReleaseClaim(chosen.ID, "gpt")
	}
	ratio := float64(counts[1]) / 10000
	if ratio < 0.72 || ratio > 0.78 {
		t.Fatalf("standard two-choice selection should be near 75%%, got %.3f", ratio)
	}
}

type fixedHealth struct {
	mu       sync.Mutex
	latency  map[int64]int64
	inflight map[int64]int64
}

func (h *fixedHealth) IsAvailable(id int64, model string) bool { return true }
func (h *fixedHealth) Claim(id int64, model string) bool {
	h.mu.Lock()
	h.inflight[id]++
	h.mu.Unlock()
	return true
}
func (h *fixedHealth) LatencyEWMA(id int64, model string) int64 { return h.latency[id] }
func (h *fixedHealth) InFlight(id int64) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inflight[id]
}
func (h *fixedHealth) RouteStats(id int64, model string) (float64, float64) {
	return float64(h.latency[id]), 1
}

func TestP2CIncludesInFlightLoad(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "busy", Priority: 1, Weight: 1},
		{ID: 2, Name: "idle", Priority: 1, Weight: 1},
	}
	h := &fixedHealth{
		latency:  map[int64]int64{1: 50, 2: 100},
		inflight: map[int64]int64{1: 10, 2: 0},
	}
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, h)
	counts := map[int64]int{}
	for i := 0; i < 5000; i++ {
		chosen, _ := scheduler.Pick(1, "gpt")
		counts[chosen.ID]++
		h.mu.Lock()
		h.inflight[chosen.ID]--
		h.mu.Unlock()
	}
	if counts[2] <= counts[1] {
		t.Fatalf("idle candidate should win load-adjusted comparisons: %+v", counts)
	}
}

func TestAllOpen(t *testing.T) {
	upstreams := []*upstream.Upstream{{ID: 1, Priority: 1, Weight: 1}}
	hm := health.New(1, time.Hour)
	hm.Report(1, "gpt", false, 0)
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)
	if _, err := scheduler.Pick(1, "gpt"); err != ErrNoUpstream {
		t.Fatalf("expected ErrNoUpstream, got %v", err)
	}
}
