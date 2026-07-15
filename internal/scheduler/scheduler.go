package scheduler

import (
	"errors"
	"math/rand"
	"sort"

	"github.com/mirainya/muxapi/internal/upstream"
)

var ErrNoUpstream = errors.New("no healthy upstream available")

// Health is the channel-level state required by the scheduler.
type Health interface {
	IsAvailable(id int64, model string) bool
	Claim(id int64, model string) bool
	LatencyEWMA(id int64, model string) int64
	InFlight(id int64) int64
	RouteStats(id int64, model string) (ewmaMs float64, succRate float64)
}

// Scheduler applies strict priority first, then standard weighted P2C inside
// the highest available tier.
type Scheduler struct {
	list   func(groupID int64) []*upstream.Upstream
	health Health
}

func New(list func(groupID int64) []*upstream.Upstream, h Health) *Scheduler {
	return &Scheduler{list: list, health: h}
}

// SetRouting remains for API compatibility. Routing now always uses standard
// P2C, so runtime smart-routing settings no longer alter the algorithm.
func (s *Scheduler) SetRouting(tolerance func() float64, enabled func() bool) {}

func (s *Scheduler) Pick(groupID int64, model string) (*upstream.Upstream, error) {
	return s.PickExcluding(groupID, model, nil)
}

func (s *Scheduler) PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, error) {
	blocked := make(map[int64]bool, len(exclude))
	for id, value := range exclude {
		blocked[id] = value
	}
	for {
		all := s.list(groupID)
		var healthy []*upstream.Upstream
		for _, candidate := range all {
			if blocked[candidate.ID] || !s.health.IsAvailable(candidate.ID, model) {
				continue
			}
			healthy = append(healthy, candidate)
		}
		if len(healthy) == 0 {
			return nil, ErrNoUpstream
		}

		topPriority := healthy[0].Priority
		for _, candidate := range healthy[1:] {
			if candidate.Priority < topPriority {
				topPriority = candidate.Priority
			}
		}
		tier := make([]*upstream.Upstream, 0, len(healthy))
		for _, candidate := range healthy {
			if candidate.Priority == topPriority {
				tier = append(tier, candidate)
			}
		}

		chosen := s.pickP2C(tier, model)
		if s.health.Claim(chosen.ID, model) {
			return chosen, nil
		}
		blocked[chosen.ID] = true
	}
}

// pickP2C performs two independent weighted draws. Drawing the same upstream
// twice is valid and returns it directly, matching standard P2C behavior.
func (s *Scheduler) pickP2C(tier []*upstream.Upstream, model string) *upstream.Upstream {
	if len(tier) == 1 {
		return tier[0]
	}
	a := weightedPick(tier)
	b := weightedPick(tier)
	if a.ID == b.ID {
		return a
	}
	baseline := medianKnownLatency(tier, func(id int64) int64 { return s.health.LatencyEWMA(id, model) })
	if p2cScore(b, s.health, model, baseline) < p2cScore(a, s.health, model, baseline) {
		return b
	}
	return a
}

func p2cScore(candidate *upstream.Upstream, health Health, model string, baseline int64) float64 {
	latency := health.LatencyEWMA(candidate.ID, model)
	if latency <= 0 {
		latency = baseline
	}
	return float64(latency) * float64(health.InFlight(candidate.ID)+1)
}

func medianKnownLatency(tier []*upstream.Upstream, latency func(id int64) int64) int64 {
	known := make([]int64, 0, len(tier))
	for _, candidate := range tier {
		if value := latency(candidate.ID); value > 0 {
			known = append(known, value)
		}
	}
	if len(known) == 0 {
		return 1
	}
	sort.Slice(known, func(i, j int) bool { return known[i] < known[j] })
	return known[len(known)/2]
}

func weightOf(candidate *upstream.Upstream) int {
	if candidate.Weight <= 0 {
		return 1
	}
	return candidate.Weight
}

func weightedPick(tier []*upstream.Upstream) *upstream.Upstream {
	if len(tier) == 1 {
		return tier[0]
	}
	total := 0
	for _, candidate := range tier {
		total += weightOf(candidate)
	}
	draw := rand.Intn(total)
	for _, candidate := range tier {
		weight := weightOf(candidate)
		if draw < weight {
			return candidate
		}
		draw -= weight
	}
	return tier[0]
}

// Share is the deterministic expected selection share for a P2C tier.
type Share struct {
	EffLatencyMs float64 `json:"eff_latency_ms"`
	Share        float64 `json:"share"`
}

// PreviewShares calculates the exact pairwise probability for two independent
// weighted draws. In-flight load is intentionally omitted from this snapshot.
func PreviewShares(tier []*upstream.Upstream, stats func(id int64) (ewmaMs, succRate float64), toleranceMs float64) map[int64]Share {
	out := make(map[int64]Share, len(tier))
	if len(tier) == 0 {
		return out
	}

	known := make([]float64, 0, len(tier))
	scores := make(map[int64]float64, len(tier))
	totalWeight := 0.0
	for _, candidate := range tier {
		latency, _ := stats(candidate.ID)
		if latency > 0 {
			known = append(known, latency)
			scores[candidate.ID] = latency
		}
		totalWeight += float64(weightOf(candidate))
	}
	baseline := 1.0
	if len(known) > 0 {
		sort.Float64s(known)
		baseline = known[len(known)/2]
	} else if toleranceMs > 0 {
		baseline = toleranceMs
	}
	for _, candidate := range tier {
		if scores[candidate.ID] <= 0 {
			scores[candidate.ID] = baseline
		}
		out[candidate.ID] = Share{EffLatencyMs: scores[candidate.ID]}
	}

	for _, first := range tier {
		pFirst := float64(weightOf(first)) / totalWeight
		for _, second := range tier {
			p := pFirst * float64(weightOf(second)) / totalWeight
			winner := first
			if scores[second.ID] < scores[first.ID] {
				winner = second
			}
			share := out[winner.ID]
			share.Share += p
			out[winner.ID] = share
		}
	}
	return out
}

// EffLatency is kept for callers that display the legacy metric.
func EffLatency(ewmaMs, succRate, toleranceMs float64) float64 {
	if ewmaMs > 0 {
		return ewmaMs
	}
	if toleranceMs > 0 {
		return toleranceMs
	}
	return 1
}
