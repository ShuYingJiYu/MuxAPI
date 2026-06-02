package scheduler

import (
	"errors"
	"math/rand"

	"github.com/mirainya/muxapi/internal/upstream"
)

var ErrNoUpstream = errors.New("no healthy upstream available")

// Health 调度层只依赖这个接口（解耦：不关心熔断如何实现）
type Health interface {
	IsAvailable(id int64) bool
}

// Scheduler 严格优先级调度 + 回切。
// 回切原理：每个请求都重新筛选健康上游并取最高优先级层，
// 高优先级上游一旦被健康层判定可用，立即重新被选中。
type Scheduler struct {
	list   func(groupID int64) []*upstream.Upstream // 某分组下启用的上游（已按 priority 升序）
	health Health
}

func New(list func(groupID int64) []*upstream.Upstream, h Health) *Scheduler {
	return &Scheduler{list: list, health: h}
}

// Pick 在指定分组内选一个上游。严格优先级：只在最高优先级的健康层内选；同层多个按权重随机。
func (s *Scheduler) Pick(groupID int64) (*upstream.Upstream, error) {
	// 1. 过滤出该分组健康的上游
	var healthy []*upstream.Upstream
	for _, u := range s.list(groupID) {
		if s.health.IsAvailable(u.ID) {
			healthy = append(healthy, u)
		}
	}
	if len(healthy) == 0 {
		return nil, ErrNoUpstream
	}
	// 2. 取最高优先级层（priority 最小值），严格优先级——绝不掺低优先级
	top := healthy[0].Priority
	var tier []*upstream.Upstream
	for _, u := range healthy {
		if u.Priority == top {
			tier = append(tier, u)
		}
	}
	// 3. 同层内按权重随机分流
	return weightedPick(tier), nil
}

func weightedPick(tier []*upstream.Upstream) *upstream.Upstream {
	if len(tier) == 1 {
		return tier[0]
	}
	total := 0
	for _, u := range tier {
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}
	r := rand.Intn(total)
	for _, u := range tier {
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		if r < w {
			return u
		}
		r -= w
	}
	return tier[0]
}
