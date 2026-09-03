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
	hm := health.New(1, 5*time.Millisecond)
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)

	chosen, lease, _ := scheduler.Pick(1, "gpt")
	if chosen.ID != 1 {
		t.Fatalf("expected primary, got %s", chosen.Name)
	}
	hm.Release(lease)
	hm.Report(1, "gpt", false, 0)

	chosen, lease, _ = scheduler.Pick(1, "gpt")
	if chosen.ID != 2 {
		t.Fatalf("expected backup while primary is open, got %s", chosen.Name)
	}
	hm.Release(lease)

	time.Sleep(10 * time.Millisecond)
	hm.ObserveProbe(1, "gpt", true, 50)
	hm.ObserveProbe(1, "gpt", true, 45)
	chosen, lease, _ = scheduler.Pick(1, "gpt")
	if chosen.ID != 1 {
		t.Fatalf("expected immediate failback to primary, got %s", chosen.Name)
	}
	hm.Release(lease)
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
	chosen, lease, _ := scheduler.Pick(1, "claude")
	if chosen.ID != 2 {
		t.Fatalf("cross-model failures should open A and select B, got %s", chosen.Name)
	}
	hm.Release(lease)
}

func TestUnsupportedModelOnlyExcludesThatModel(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1},
		{ID: 2, Name: "B", Priority: 2, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	hm.MarkModelUnsupported(1, "gpt-5.6")
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)

	chosen, lease, _ := scheduler.Pick(1, "gpt-5.6")
	if chosen.ID != 2 {
		t.Fatalf("unsupported model should use B, got %s", chosen.Name)
	}
	hm.Release(lease)
	chosen, lease, _ = scheduler.Pick(1, "gpt-5.5")
	if chosen.ID != 1 {
		t.Fatalf("other models should still use A, got %s", chosen.Name)
	}
	hm.Release(lease)
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
		chosen, lease, err := scheduler.Pick(1, "gpt")
		if err != nil {
			t.Fatal(err)
		}
		counts[chosen.ID]++
		hm.Release(lease)
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
		chosen, lease, _ := scheduler.Pick(1, "gpt")
		counts[chosen.ID]++
		hm.Release(lease)
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
func (h *fixedHealth) Claim(_ int64, id int64, _ string) (health.Lease, bool) {
	h.mu.Lock()
	h.inflight[id]++
	h.mu.Unlock()
	return health.Lease{UpstreamID: id, Token: uint64(id)}, true
}
func (h *fixedHealth) ClaimLastResort(int64, int64, string) (health.Lease, bool) {
	return health.Lease{}, false
}
func (h *fixedHealth) RecoveryInfo(int64) health.RecoveryCandidate {
	return health.RecoveryCandidate{}
}
func (h *fixedHealth) LatencyEWMA(id int64) int64 { return h.latency[id] }
func (h *fixedHealth) InFlight(id int64) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inflight[id]
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
		chosen, _, _ := scheduler.Pick(1, "gpt")
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
	chosen, lease, err := scheduler.Pick(1, "gpt")
	if err != nil || chosen == nil || !lease.Valid() {
		t.Fatalf("single open channel should receive one last-route trial, chosen=%v err=%v", chosen, err)
	}
	hm.Complete(lease, health.ResultFailure, 0)
	if _, _, err := scheduler.Pick(1, "gpt"); err != ErrNoUpstream {
		t.Fatalf("failed last-route trial must respect cooldown, got %v", err)
	}
}

func TestLastRouteTrialUsesMostRecentlySuccessfulChannel(t *testing.T) {
	upstreams := []*upstream.Upstream{
		{ID: 1, Priority: 1, Weight: 1},
		{ID: 2, Priority: 1, Weight: 1},
		{ID: 3, Priority: 1, Weight: 1},
	}
	hm := health.New(1, time.Hour)
	for _, id := range []int64{1, 2, 3} {
		hm.Report(id, "gpt", true, 10)
		time.Sleep(time.Millisecond)
	}
	for _, id := range []int64{1, 2, 3} {
		hm.Report(id, "gpt", false, 0)
	}
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)
	chosen, lease, err := scheduler.Pick(9, "gpt")
	if err != nil || chosen.ID != 3 {
		t.Fatalf("last-route trial should prefer the latest success, chosen=%v err=%v", chosen, err)
	}
	hm.Release(lease)
}

func TestLastRouteTrialNeverRetriesExcludedChannel(t *testing.T) {
	upstreams := []*upstream.Upstream{{ID: 1, Priority: 1, Weight: 1}}
	hm := health.New(1, time.Hour)
	hm.Report(1, "gpt", false, 0)
	scheduler := New(func(int64) []*upstream.Upstream { return upstreams }, hm)
	if _, _, err := scheduler.PickExcluding(1, "gpt", map[int64]bool{1: true}); err != ErrNoUpstream {
		t.Fatalf("excluded channel was retried as last route: %v", err)
	}
}
