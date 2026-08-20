// Package scheduler 在最高可用优先级层内使用加权 P2C 选择上游。
package scheduler

import (
	"errors"
	"math/rand"
	"sort"

	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// ErrNoUpstream 表示分组中没有可声明占用的健康渠道。
var ErrNoUpstream = errors.New("no healthy upstream available")

// Health is the channel-level state required by the scheduler. Model only
// affects availability via the capability cache; latency and load are per
// channel.
type Health interface {
	IsAvailable(id int64, model string) bool
	Claim(id int64, model string) bool
	LatencyEWMA(id int64) int64
	InFlight(id int64) int64
}

// Scheduler applies strict priority first, then standard weighted P2C inside
// the highest available tier.
type Scheduler struct {
	list    func(groupID int64) []*upstream.Upstream
	health  Health
	routing *intelligentRouter
}

// New 创建调度器；list 在每次选择时读取最新分组成员。
func New(list func(groupID int64) []*upstream.Upstream, h Health) *Scheduler {
	return &Scheduler{list: list, health: h}
}

// SetIntelligentRouting enables the cost/cache-aware selector. The ordinary
// priority/P2C path remains the fallback whenever pricing or history is not
// sufficient to make a defensible cost decision.
func (s *Scheduler) SetIntelligentRouting(st *store.Store, config func() routing.Config) {
	if st == nil {
		s.routing = nil
		return
	}
	if config == nil {
		defaults := routing.DefaultConfig()
		config = func() routing.Config { return defaults }
	}
	s.routing = &intelligentRouter{store: st, config: config}
}

// PickWithFeatures is the optional forwarder hook for intelligent routing.
// It deliberately keeps the existing Picker interface unchanged for callers
// and tests that only need ordinary scheduling.
func (s *Scheduler) PickWithFeatures(groupID int64, model string, features routing.RequestFeatures, exclude map[int64]bool) (*upstream.Upstream, routing.Decision, error) {
	if s.routing == nil {
		candidate, err := s.PickExcluding(groupID, model, exclude)
		return candidate, routing.Decision{}, err
	}
	return s.routing.pick(s, groupID, model, features, exclude)
}

// Pick 选择一个上游，并在健康管理器中声明并发占用。
func (s *Scheduler) Pick(groupID int64, model string) (*upstream.Upstream, error) {
	return s.PickExcluding(groupID, model, nil)
}

// PickExcluding 排除已尝试渠道，供转发层故障切换时重复调用。
func (s *Scheduler) PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, error) {
	blocked := make(map[int64]bool, len(exclude))
	for id, value := range exclude {
		blocked[id] = value
	}
	// 成员清单每次调用只读一次库：重试只为处理 Claim 竞争，与成员变化无关。
	// 转发层每次换源都会重新调用本函数，故后台增删仍是下个请求即生效。
	all := s.list(groupID)
	for {
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

		// IsAvailable 与 Claim 分离，声明失败时重新选择可处理并发半开竞争。
		chosen := s.pickP2C(tier)
		if s.health.Claim(chosen.ID, model) {
			return chosen, nil
		}
		blocked[chosen.ID] = true
	}
}

// pickP2C performs two independent weighted draws. Drawing the same upstream
// twice is valid and returns it directly, matching standard P2C behavior.
func (s *Scheduler) pickP2C(tier []*upstream.Upstream) *upstream.Upstream {
	if len(tier) == 1 {
		return tier[0]
	}
	a := weightedPick(tier)
	b := weightedPick(tier)
	if a.ID == b.ID {
		return a
	}
	baseline := medianKnownLatency(tier, s.health.LatencyEWMA)
	if p2cScore(b, s.health, baseline) < p2cScore(a, s.health, baseline) {
		return b
	}
	return a
}

// p2cScore 用延迟乘以当前并发数，使慢渠道和繁忙渠道自然降低胜率。
func p2cScore(candidate *upstream.Upstream, health Health, baseline int64) float64 {
	latency := health.LatencyEWMA(candidate.ID)
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
