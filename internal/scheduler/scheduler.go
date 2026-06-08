package scheduler

import (
	"errors"
	"math/rand"

	"github.com/mirainya/muxapi/internal/upstream"
)

var ErrNoUpstream = errors.New("no healthy upstream available")

// 智能路由内部参数（焊死，不对用户暴露）：
//   - succRateFloor：成功率地板，全失败渠道有效延迟极大但有限，防 (1-p)/p 除零爆 inf
//   - effWeightFloor：权重地板，保证再慢的渠道也留微量流量探测保活，永不归零（归零交给熔断）
const (
	succRateFloor  = 0.01
	effWeightFloor = 1e-6
	// coldSuccThreshold 成功率≥此值视为「无失败记录」(真冷启动)，否则视为有失败的死/半死渠道。
	coldSuccThreshold = 0.9999
)

// Health 调度层只依赖这个接口（解耦：不关心熔断如何实现）
type Health interface {
	IsAvailable(id int64, model string) bool
	// LatencyEWMA 返回该 (上游,模型) 成功延迟的 EWMA(ms)，供同层 P2C 选路比较；0=未知。
	LatencyEWMA(id int64, model string) int64
	// RouteStats 返回该 (上游,模型) 的成功延迟 EWMA(ms) 与成功率(0..1)，供延迟加权选路算「有效延迟」。
	// ewmaMs=0 表示延迟未知(冷启动)；succRate 无样本时乐观返回 1。
	RouteStats(id int64, model string) (ewmaMs float64, succRate float64)
}

// Scheduler 严格优先级调度 + 回切。
// 回切原理：每个请求都重新筛选健康上游并取最高优先级层，
// 高优先级上游一旦被健康层判定可用，立即重新被选中。
type Scheduler struct {
	list   func(groupID int64) []*upstream.Upstream // 某分组下启用的上游（已按 priority 升序）
	health Health
	// 智能路由(延迟加权选路)：默认关——退回经典 P2C；由 main 注入 SetRouting 后开启。
	tolerance func() float64 // 容忍线(ms)，既是单请求超时上限、又是算有效延迟时的「失败成本」
	enabled   func() bool    // 总开关；nil 视为关闭
}

func New(list func(groupID int64) []*upstream.Upstream, h Health) *Scheduler {
	return &Scheduler{list: list, health: h}
}

// SetRouting 注入智能路由(延迟加权选路)的运行时配置：容忍线 + 总开关。
// 启动时一次性设置，二者读 settings 即时生效。enabled 返回 false 时退回经典 P2C。
func (s *Scheduler) SetRouting(tolerance func() float64, enabled func() bool) {
	s.tolerance = tolerance
	s.enabled = enabled
}

// routingOn 智能路由是否开启（未注入或开关关闭→否，走经典 P2C）。
func (s *Scheduler) routingOn() bool {
	return s.enabled != nil && s.enabled()
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
	all := s.list(groupID)
	var healthy []*upstream.Upstream
	for _, u := range all {
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
	// 3. 同层内选路：智能路由开 → 有效延迟加权抽样；否则经典 P2C（延迟感知的 2 选 1）
	var chosen *upstream.Upstream
	if s.routingOn() {
		chosen = s.pickWeightedByEffLatency(tier, model)
	} else {
		chosen = s.pickP2C(tier, model)
	}
	return chosen, nil
}

// pickWeightedByEffLatency 智能路由：按「有效延迟」加权抽样（值越小被选概率越大）。
//
//	有效延迟 = 成功延迟EWMA + (1-成功率)/成功率 × 容忍线T
//
// 物理意义：成功率 p 的渠道平均要白挨 (1-p)/p 次超时(每次烧 T)才中一次，
// 把「重试浪费的时间」折进来——这才是用户真实的期望等待。于是「成功但巨慢」
// 或「快但常失败」都自动垫底，无需独立判定成功率。
//
// 选中权重 = 配置权重 / 有效延迟，带地板永不归零(软降权，归零的活交给熔断)；
// 慢渠道只是少拿流量、仍留微量探测保活，变快了权重自动回升。
// 冷启动(EWMA=0)用同层已知最优延迟代入，与最快者公平竞争去探数据；全冷则退化为按配置权重随机。
func (s *Scheduler) pickWeightedByEffLatency(tier []*upstream.Upstream, model string) *upstream.Upstream {
	if len(tier) == 1 {
		return tier[0]
	}
	shares := PreviewShares(tier, func(id int64) (float64, float64) { return s.health.RouteStats(id, model) }, s.tolerance())
	// 按占比随机抽样（占比逻辑在 PreviewShares 单源实现，此处只做抽签）
	r := rand.Float64()
	for _, u := range tier {
		sh := shares[u.ID].Share
		if r < sh {
			return u
		}
		r -= sh
	}
	return tier[len(tier)-1]
}

// Share 一个候选的流量分配预览（确定性，与 Pick 同源）。
type Share struct {
	EffLatencyMs float64 `json:"eff_latency_ms"` // 有效延迟(ms)，越小越优先
	Share        float64 `json:"share"`          // 归一化占比 0..1，同层加和=1
}

// PreviewShares 算同优先级层各候选的流量分配占比——旁路只读，不影响 Pick 的随机性。
// 与 pickWeightedByEffLatency 同源：同样的有效延迟公式、冷启动(代入同层最优)、权重地板处理。
// stats 提供每个候选的(成功延迟EWMA ms, 成功率)；toleranceMs 为容忍线。供 server 预览、前端展示。
func PreviewShares(tier []*upstream.Upstream, stats func(id int64) (ewmaMs, succRate float64), toleranceMs float64) map[int64]Share {
	out := make(map[int64]Share, len(tier))
	if len(tier) == 0 {
		return out
	}
	if toleranceMs <= 0 {
		// 容忍线兜底：生产注入口恒正(不可达)，防全冷启动时 base=toleranceMs=0
		// → 权重 weight/0=+Inf → 归一化 Inf/Inf=NaN。取正最小值保结果有限。
		toleranceMs = 1
	}
	// 一遍：算已知有效延迟 + 记同层最优(给冷启动代入)；存 sr 供二遍区分「全失败 vs 真冷启动」
	effs := make([]float64, len(tier))
	srs := make([]float64, len(tier))
	minKnown := -1.0
	for i, u := range tier {
		ewma, sr := stats(u.ID)
		srs[i] = sr
		if ewma <= 0 {
			effs[i] = 0 // 无成功延迟样本：二遍据 sr 区分真冷启动 / 全失败
			continue
		}
		eff := EffLatency(ewma, sr, toleranceMs)
		effs[i] = eff
		if minKnown < 0 || eff < minKnown {
			minKnown = eff
		}
	}
	// 基线延迟：同层最优已知；全冷则用容忍线(给「全失败渠道」一个合理延迟基底，不致除零)
	base := minKnown
	if base <= 0 {
		base = toleranceMs
	}
	// 二遍：区分两种「无成功样本」——
	//   sr≈1(无失败记录)=真冷启动 → 乐观代入同层最优，公平竞争抢流量探活；
	//   sr<1(有失败无成功)=死/半死渠道 → EffLatency(基线, sr) 让低成功率推高有效延迟、占比趋零。
	// 这样「全失败」不再被误当冷启动给最低延迟(修复看板自相矛盾，也让真实选路正确给死渠道降权)。
	weights := make([]float64, len(tier))
	total := 0.0
	for i := range tier {
		if effs[i] == 0 {
			if srs[i] >= coldSuccThreshold {
				effs[i] = base // 真冷启动：乐观
			} else {
				effs[i] = EffLatency(base, srs[i], toleranceMs) // 有失败：按成功率惩罚
			}
		}
		w := float64(weightOf(tier[i])) / effs[i]
		if w < effWeightFloor {
			w = effWeightFloor // 地板：再差也留微量流量保活探测
		}
		weights[i] = w
		total += w
	}
	for i, u := range tier {
		sh := 0.0
		if total > 0 {
			sh = weights[i] / total
		}
		out[u.ID] = Share{EffLatencyMs: effs[i], Share: sh}
	}
	return out
}

// EffLatency 有效延迟(ms)：成功延迟 + 失败的期望超时成本。succRate 夹到 (0,1] 防除零。
// 导出供 server 预览流量占比，与选路用同一公式——口径单一来源，前端展示=实际选路。
func EffLatency(ewmaMs, succRate, toleranceMs float64) float64 {
	if toleranceMs <= 0 {
		toleranceMs = 1 // 容忍线兜底：恒正不可达，防失败成本项归零失真（见 PreviewShares）
	}
	if succRate > 1 {
		succRate = 1
	}
	if succRate < succRateFloor {
		succRate = succRateFloor // 成功率地板：全失败渠道有效延迟极大但有限(否则除零→inf)
	}
	return ewmaMs + (1-succRate)/succRate*toleranceMs
}

func weightOf(u *upstream.Upstream) int {
	if u.Weight <= 0 {
		return 1
	}
	return u.Weight
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
