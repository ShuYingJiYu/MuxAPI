package health

import (
	"sort"
	"sync"
	"time"
)

// State 熔断状态
type State int

const (
	Closed   State = iota // 正常
	Open                  // 熔断冷却中
	HalfOpen              // 半开探测中
)

func (s State) String() string {
	switch s {
	case Open:
		return "OPEN"
	case HalfOpen:
		return "HALF_OPEN"
	default:
		return "CLOSED"
	}
}

// breaker 单个上游的熔断器（仅内存）
type breaker struct {
	state       State
	fails       int       // 连续失败次数
	openUntil   time.Time // Open：冷却到期时间；HalfOpen：本次试探的超时时间（防闸门空占永久卡死）
	probing     bool      // 半开单请求闸门：已放出一个试探请求、其余业务请求一律拦住，直到该试探有结果
	lastProbe   time.Time
	latencyMs   int64
	latencyEWMA float64 // 成功请求延迟的指数加权移动平均(ms)，供 P2C 选路；0=尚无数据
	// 业务流量统计（仅转发层计入，探测不算；进程级、重启清零）
	reqs       int64 // 总请求数
	failReqs   int64 // 失败请求数
	totLatency int64 // 累计延迟(ms)，仅成功请求
	// 趋势采样：环形缓冲 + 上次采样时的累计值（用于算窗口增量）
	trend     []TrendPoint
	lastReqs  int64
	lastFails int64
}

// TrendPoint 一次采样点：该窗口的状态/延迟/成功率，供看板画 uptime 栅栏。
type TrendPoint struct {
	TS       int64   `json:"ts"`        // unix 秒
	Status   int     `json:"status"`    // 0=无数据 1=正常 2=降级 3=熔断
	LatMs    int64   `json:"lat_ms"`    // 当前延迟
	SuccRate float64 `json:"succ_rate"` // 本窗口成功率 0..1
}

// 栅栏状态档位
const (
	statNoData   = 0 // 灰：从未观测到
	statOK       = 1 // 绿：健康
	statDegraded = 2 // 橙：有失败但仍在服务 / 半开试探
	statDown     = 3 // 红：熔断中
)

const trendCap = 60 // 环形缓冲容量

// ewmaAlpha EWMA 平滑系数：新样本权重 0.3、历史权重 0.7。
// 取值越大越灵敏（更快跟上延迟变化、但易被偶发抖动带偏），越小越平滑（更稳、但反应慢）。
// 0.3 是常见折中：约 3~4 个样本就能体现一次趋势变化，又能压住单次毛刺。
const ewmaAlpha = 0.3

// breakerKey 熔断键：(上游, 模型)。model=="" 表示「上游级」键——
// 承载凭证/余额类(401/402/403)的整上游熔断，以及看板的上游级流量统计聚合。
// model!="" 表示「模型级」键——承载 429/5xx 这类某上游某模型的局部故障，
// 使「gpt-5.5 挂了」不再连累同上游的 claude。
type breakerKey struct {
	upstreamID int64
	model      string
}

// AlertEvent 一次熔断状态翻转事件（载荷已在锁内拷出，可安全异步发送）。
type AlertEvent struct {
	UpstreamID int64  `json:"upstream_id"`
	Model      string `json:"model"` // ""=上游级
	FromState  string `json:"from_state"`
	ToState    string `json:"to_state"`
	Fails      int    `json:"fails"`
	TS         int64  `json:"ts"` // unix 秒
}

// Alerter 告警钩子：drive 检测到翻转后，由调用方在释放 m.mu 后 go Notify。
// 实现须自行保证非阻塞安全（内部带超时、失败不 panic）。
type Alerter interface {
	Notify(ev AlertEvent)
}

// Manager 健康管理：调度层只依赖 IsAvailable，转发层调 Report。
type Manager struct {
	mu            sync.Mutex
	breakers      map[breakerKey]*breaker
	failThreshold int
	cooldown      time.Duration
	alerter       Alerter // 可选告警钩子；nil 表示不告警
}

func New(failThreshold int, cooldown time.Duration) *Manager {
	return &Manager{
		breakers:      make(map[breakerKey]*breaker),
		failThreshold: failThreshold,
		cooldown:      cooldown,
	}
}

// SetAlerter 注入告警钩子（启动时一次性设置，运行期不再改）。
func (m *Manager) SetAlerter(a Alerter) { m.alerter = a }

func (m *Manager) get(k breakerKey) *breaker {
	b := m.breakers[k]
	if b == nil {
		b = &breaker{state: Closed}
		m.breakers[k] = b
	}
	return b
}

// canServe 只读判定：该键现在能否承接【一个】业务请求（调用方须持有 m.mu）。
// 半开单请求闸门的「判定」阶段——不在此占用闸门（占用在 markServed），
// 避免 scheduler 过滤多个候选时，对最终未被选中的上游误占名额。
//
//   - Closed：可服务。
//   - Open 未到冷却：不可服务（死渠道不吃业务流量）。
//   - Open 冷却到期：翻 HalfOpen 并清空闸门，准备放一个试探 → 可服务。
//   - HalfOpen 且闸门空闲：可服务（这一个就是试探）。
//   - HalfOpen 且闸门占用：默认不可服务；但若试探已超时(openUntil 已过)，
//     说明上一个试探请求没回执(被 scheduler 落选/未发出)，强制释放闸门重新放行，
//     防止业务永久不再试探、又恰好无探测覆盖的死渠道卡死。
func (m *Manager) canServe(b *breaker) bool {
	if b.state == Open {
		if !time.Now().After(b.openUntil) {
			return false
		}
		b.state = HalfOpen // 冷却结束，准备放一个试探
		b.probing = false
	}
	if b.state == HalfOpen && b.probing {
		return time.Now().After(b.openUntil) // 试探超时则重新放行，否则拦住
	}
	return true
}

// IsAvailable 调度层询问：该上游的该模型现在能用吗？
// 双层判定：上游级(凭证类故障)与模型级须都能服务。
// 两阶段提交：先 canServe 全通过，再 markServed 占用半开闸门——
// 避免「上游级放行但模型级拦截」时空占上游级的试探名额。
func (m *Manager) IsAvailable(id int64, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	bs := []*breaker{m.get(breakerKey{id, ""})} // 上游级：凭证/余额坏了，所有模型连坐
	if model != "" {
		bs = append(bs, m.get(breakerKey{id, model})) // 模型级：仅该模型局部故障
	}
	for _, b := range bs {
		if !m.canServe(b) {
			return false
		}
	}
	m.markServed(bs)
	return true
}

// markServed 占用闸门：对处于 HalfOpen 的键标记「试探在途」，并以 openUntil 记下试探超时
// (= now + cooldown)，供 canServe 的空占自愈用。调用方须持有 m.mu。
func (m *Manager) markServed(bs []*breaker) {
	now := time.Now()
	for _, b := range bs {
		if b.state == HalfOpen && !b.probing {
			b.probing = true
			b.openUntil = now.Add(m.cooldown)
		}
	}
}

// LatencyEWMA 返回该 (上游,模型) 成功延迟的 EWMA(ms)，供调度层 P2C 比较。
// 优先取模型级键（与第二步选路粒度一致）；该粒度尚无数据时回退上游级键。
// 返回 0 表示「未知」——调度层据此给新上游探数据的机会（视为最优）。
func (m *Manager) LatencyEWMA(id int64, model string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if model != "" {
		if b := m.breakers[breakerKey{id, model}]; b != nil && b.latencyEWMA > 0 {
			return int64(b.latencyEWMA)
		}
	}
	if b := m.breakers[breakerKey{id, ""}]; b != nil {
		return int64(b.latencyEWMA)
	}
	return 0
}

// Report 转发层反馈业务请求结果：驱动熔断状态机 + 计入流量统计。
// model 为空时驱动上游级键（凭证类失败应熔断整上游）；非空时驱动该模型键。
// 无论哪种，流量统计都累计到上游级键，供看板按上游聚合展示。
func (m *Manager) Report(id int64, model string, ok bool, latencyMs int64) {
	m.mu.Lock()
	up := m.get(breakerKey{id, ""}) // 上游级：承载看板统计
	up.reqs++
	if !ok {
		up.failReqs++
	} else if latencyMs > 0 {
		up.totLatency += latencyMs
		up.latencyMs = latencyMs
	}
	// 状态机驱动到对应粒度的键：model=="" → 上游级；否则 → 模型级
	b := m.get(breakerKey{id, model})
	from, to := m.drive(b, ok, latencyMs)
	ev, flipped := transitionEvent(id, model, from, to, b.fails)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// reportProbe 主动探测反馈：只驱动熔断状态机，不计入业务统计。
func (m *Manager) reportProbe(id int64, model string, ok bool, latencyMs int64) {
	m.mu.Lock()
	b := m.get(breakerKey{id, model})
	from, to := m.drive(b, ok, latencyMs)
	ev, flipped := transitionEvent(id, model, from, to, b.fails)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// ObserveProbe 供外部探测器(monitor)反馈一次探测结果：标记探测时刻 + 驱动熔断状态机，
// 不计入业务流量统计。是 markProbe+reportProbe 的导出合并版——探测系统统一后，
// monitor 探测器是唯一主动探测源，一次探测既记看板(Record)又驱动熔断(本方法)。
// scope 由调用方按熔断器口径决定：凭证类(401/402/403)传 model="" 熔整上游，其余传具体 model。
func (m *Manager) ObserveProbe(id int64, model string, ok bool, latencyMs int64) {
	m.markProbe(id, model)
	m.reportProbe(id, model, ok, latencyMs)
}

// dispatch 异步派发告警：drive 持锁、此处已释放锁，仍 go 出去防 webhook 慢阻塞调用方。
func (m *Manager) dispatch(ev AlertEvent) {
	if m.alerter == nil {
		return
	}
	go m.alerter.Notify(ev)
}

// transitionEvent 仅在「真正翻转」时构造事件：
// Closed→Open=熔断、(Open|HalfOpen)→Closed=恢复；HalfOpen 中间态与同态不发。
// 载荷字段均为值拷贝，可安全带出锁外异步发送。
func transitionEvent(id int64, model string, from, to State, fails int) (AlertEvent, bool) {
	flip := (from == Closed && to == Open) || (from != Closed && to == Closed)
	if !flip {
		return AlertEvent{}, false
	}
	return AlertEvent{
		UpstreamID: id, Model: model,
		FromState: from.String(), ToState: to.String(),
		Fails: fails, TS: time.Now().Unix(),
	}, true
}

// drive 熔断状态机（调用方须持有 m.mu）。返回 (旧状态, 新状态) 供调用方判翻转。
// 无论成功失败，只要有结果回来就复位半开闸门(probing=false)——本次试探已落地，
// 闸门交还：成功则随 Closed 一并清空，失败则让下一个冷却周期能再放一个试探。
func (m *Manager) drive(b *breaker, ok bool, latencyMs int64) (from, to State) {
	from = b.state
	b.probing = false
	if ok {
		b.fails = 0
		b.state = Closed
		if latencyMs > 0 {
			b.latencyMs = latencyMs
			// 仅成功请求计入 EWMA（失败延迟无意义）。首次直接赋值，避免被 0 拖低。
			if b.latencyEWMA == 0 {
				b.latencyEWMA = float64(latencyMs)
			} else {
				b.latencyEWMA = ewmaAlpha*float64(latencyMs) + (1-ewmaAlpha)*b.latencyEWMA
			}
		}
		return from, b.state
	}
	b.fails++
	if b.fails >= m.failThreshold || b.state == HalfOpen {
		b.state = Open
		b.openUntil = time.Now().Add(m.cooldown)
	}
	return from, b.state
}

// Snapshot 健康看板用：返回各上游状态快照。
type Snapshot struct {
	State     string
	Fails     int
	LatencyMs int64
	OpenUntil time.Time
	LastProbe time.Time
	// 业务流量统计
	Reqs     int64        // 总请求数
	FailReqs int64        // 失败请求数
	SuccRate float64      // 成功率 0..1（无请求时为 0）
	AvgLatMs int64        // 成功请求平均延迟(ms)
	Trend    []TrendPoint // 趋势采样序列（旧→新）
}

// Snapshot 健康看板用：返回该上游的「上游级」键快照（按上游聚合，流量统计在此累计）。
func (m *Manager) Snapshot(id int64) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(breakerKey{id, ""})
	sn := Snapshot{
		State: b.state.String(), Fails: b.fails, LatencyMs: b.latencyMs,
		OpenUntil: b.openUntil, LastProbe: b.lastProbe,
		Reqs: b.reqs, FailReqs: b.failReqs,
		Trend: append([]TrendPoint(nil), b.trend...), // 拷贝，避免外部读到并发改的底层数组
	}
	if b.reqs > 0 {
		sn.SuccRate = float64(b.reqs-b.failReqs) / float64(b.reqs)
	}
	if succ := b.reqs - b.failReqs; succ > 0 {
		sn.AvgLatMs = b.totLatency / succ
	}
	return sn
}

// ModelHealth 模型级健康精简状态（看板按上游展开模型徽章用，不含趋势数组省带宽）。
type ModelHealth struct {
	Model     string `json:"model"`      // 模型名
	State     string `json:"state"`      // CLOSED 正常 / OPEN 熔断 / HALF_OPEN 半开
	Fails     int    `json:"fails"`      // 当前连续失败数
	LatencyMs int64  `json:"latency_ms"` // 最近一次延迟(ms)
	LastProbe int64  `json:"last_probe"` // 最后探测 unix 秒，0=从未探测
}

// ModelStates 返回该上游下所有模型级(model!="")键的精简状态，按模型名排序输出稳定。
// 只读：持锁拷贝返回，避免外部读到并发改的内部状态。上游无模型级键时返回空切片。
func (m *Manager) ModelStates(id int64) []ModelHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelHealth, 0)
	for k, b := range m.breakers {
		if k.upstreamID != id || k.model == "" {
			continue
		}
		var lastProbe int64
		if !b.lastProbe.IsZero() {
			lastProbe = b.lastProbe.Unix()
		}
		out = append(out, ModelHealth{
			Model: k.model, State: b.state.String(), Fails: b.fails,
			LatencyMs: b.latencyMs, LastProbe: lastProbe,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (m *Manager) markProbe(id int64, model string) {
	m.mu.Lock()
	m.get(breakerKey{id, model}).lastProbe = time.Now()
	m.mu.Unlock()
}

// Sample 给所有已知上游打一个趋势采样点：用自上次采样以来的请求增量算窗口成功率。
// 建议由 sampler goroutine 周期调用。
func (m *Manager) Sample() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	for _, b := range m.breakers {
		dReqs := b.reqs - b.lastReqs
		dFails := b.failReqs - b.lastFails
		rate := 1.0 // 本窗口无流量时按状态机判定，不强行算成功
		if dReqs > 0 {
			rate = float64(dReqs-dFails) / float64(dReqs)
		}
		b.lastReqs, b.lastFails = b.reqs, b.failReqs

		// 状态优先看熔断器：Open=红，HalfOpen/本窗口有失败=橙；
		// 闭合且曾被观测过(有过流量或探测) → 绿，即使本窗口空闲也算健康（栅栏保持连续绿）；
		// 从头到尾没观测过 → 灰。
		status := statOK
		switch {
		case b.state == Open:
			status = statDown
		case b.state == HalfOpen || (dReqs > 0 && rate < 1):
			status = statDegraded
		case b.reqs == 0 && b.lastProbe.IsZero():
			status = statNoData
		}
		b.trend = append(b.trend, TrendPoint{TS: now, Status: status, LatMs: b.latencyMs, SuccRate: rate})
		if len(b.trend) > trendCap {
			b.trend = b.trend[len(b.trend)-trendCap:]
		}
	}
}
