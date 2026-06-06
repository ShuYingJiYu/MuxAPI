package health

import (
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
	state     State
	fails     int       // 连续失败次数
	openUntil time.Time // 冷却到期时间
	lastProbe time.Time
	latencyMs int64
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

// isOpen 判断单个键当前是否处于 Open（冷却中）；冷却到期自动转半开放行。
// 调用方须持有 m.mu。
func (m *Manager) isOpen(k breakerKey) bool {
	b := m.get(k)
	if b.state == Open && time.Now().After(b.openUntil) {
		b.state = HalfOpen // 冷却结束，放一个请求/探测试探
	}
	return b.state == Open
}

// IsAvailable 调度层询问：该上游的该模型现在能用吗？
// 双层判定：上游级(凭证类故障)与模型级任一 Open 即不可用。
func (m *Manager) IsAvailable(id int64, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isOpen(breakerKey{id, ""}) { // 上游级：凭证/余额坏了，所有模型连坐
		return false
	}
	if model != "" && m.isOpen(breakerKey{id, model}) { // 模型级：仅该模型局部故障
		return false
	}
	return true
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
func (m *Manager) drive(b *breaker, ok bool, latencyMs int64) (from, to State) {
	from = b.state
	if ok {
		b.fails = 0
		b.state = Closed
		if latencyMs > 0 {
			b.latencyMs = latencyMs
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
