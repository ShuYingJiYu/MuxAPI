package scheduler

import (
	"errors"
	"math/rand"

	"github.com/mirainya/muxapi/internal/upstream"
)

var ErrNoUpstream = errors.New("no healthy upstream available")

// Health 调度层只依赖这个接口（解耦：不关心熔断如何实现）
type Health interface {
	IsAvailable(id int64, model string) bool
	// LatencyEWMA 返回该 (上游,模型) 成功延迟的 EWMA(ms)，供同层 P2C 选路比较；0=未知。
	LatencyEWMA(id int64, model string) int64
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
func (s *Scheduler) Pick(groupID int64, model string) (*upstream.Upstream, error) {
	return s.PickExcluding(groupID, model, nil)
}

// PickExcluding 同 Pick，但跳过 exclude 中的上游（本次请求已试过的）。
// 单次请求内失败重试用它「立即换下一个」，与熔断阈值无关——
// 熔断负责跨请求的长期摘除，单请求重试只需避开本次已试过的。
// model 用于按 (上游,模型) 粒度判定健康：某上游的某模型熔断，不影响该上游的其他模型。
func (s *Scheduler) PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, error) {
	// 1. 过滤出该分组健康、且未被本次请求排除的上游
	var healthy []*upstream.Upstream
	for _, u := range s.list(groupID) {
		if exclude[u.ID] {
			continue
		}
		if s.health.IsAvailable(u.ID, model) {
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
	// 3. 同层内 P2C（power-of-two-choices）延迟感知选路
	return s.pickP2C(tier, model), nil
}

// pickP2C 在同优先级层内做延迟感知选择：
// 按权重随机抽 2 个不同候选，选 EWMA 延迟较低者；只剩 1 个则直选。
//
// 为何不「永远选最快」：单点贪心会把全部流量灌给当前最快的上游，
// 反而把它压垮、延迟飙升后又集体倒戈下一个——羊群效应导致负载震荡。
// P2C 只在「随机两个」里挑较优，天然分散负载、避免共振，是经典的负载均衡折中。
//
// 冷启动：EWMA==0 表示该上游尚无成功样本，视为「最优」（返回较小者时它必胜），
// 主动给新上游放一点流量去探延迟数据，否则它永远抢不到样本、永远是 0。
func (s *Scheduler) pickP2C(tier []*upstream.Upstream, model string) *upstream.Upstream {
	if len(tier) == 1 {
		return tier[0]
	}
	a := weightedPick(tier)
	// 抽第二个，要求与 a 不同（最多重试几次，避免权重悬殊时死循环）
	var b *upstream.Upstream
	for i := 0; i < 4; i++ {
		c := weightedPick(tier)
		if c.ID != a.ID {
			b = c
			break
		}
	}
	if b == nil { // 极端权重下没抽到不同的，退化为单选
		return a
	}
	// EWMA==0(未知) 视为延迟极低 → 冷启动上游优先被选中探数据
	la, lb := s.ewmaOrBest(a.ID, model), s.ewmaOrBest(b.ID, model)
	if lb < la {
		return b
	}
	return a
}

// ewmaOrBest 取延迟 EWMA；未知(0)映射为 -1，比任何真实延迟都小 → 冷启动视为最优。
func (s *Scheduler) ewmaOrBest(id int64, model string) int64 {
	if e := s.health.LatencyEWMA(id, model); e > 0 {
		return e
	}
	return -1
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
