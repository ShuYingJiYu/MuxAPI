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

// Manager 健康管理：调度层只依赖 IsAvailable，转发层调 Report。
type Manager struct {
	mu            sync.Mutex
	breakers      map[int64]*breaker
	failThreshold int
	cooldown      time.Duration
}

func New(failThreshold int, cooldown time.Duration) *Manager {
	return &Manager{
		breakers:      make(map[int64]*breaker),
		failThreshold: failThreshold,
		cooldown:      cooldown,
	}
}

func (m *Manager) get(id int64) *breaker {
	b := m.breakers[id]
	if b == nil {
		b = &breaker{state: Closed}
		m.breakers[id] = b
	}
	return b
}

// IsAvailable 调度层询问：该上游现在能用吗？冷却到期自动转半开。
func (m *Manager) IsAvailable(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	if b.state == Open && time.Now().After(b.openUntil) {
		b.state = HalfOpen // 冷却结束，放一个请求/探测试探
	}
	return b.state != Open
}

// Report 转发层反馈业务请求结果：驱动熔断状态机 + 计入流量统计。
func (m *Manager) Report(id int64, ok bool, latencyMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	b.reqs++
	if !ok {
		b.failReqs++
	} else if latencyMs > 0 {
		b.totLatency += latencyMs
	}
	m.drive(b, ok, latencyMs)
}

// reportProbe 主动探测反馈：只驱动熔断状态机，不计入业务统计。
func (m *Manager) reportProbe(id int64, ok bool, latencyMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drive(m.get(id), ok, latencyMs)
}

// drive 熔断状态机（调用方须持有 m.mu）。
func (m *Manager) drive(b *breaker, ok bool, latencyMs int64) {
	if ok {
		b.fails = 0
		b.state = Closed
		if latencyMs > 0 {
			b.latencyMs = latencyMs
		}
		return
	}
	b.fails++
	if b.fails >= m.failThreshold || b.state == HalfOpen {
		b.state = Open
		b.openUntil = time.Now().Add(m.cooldown)
	}
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

func (m *Manager) Snapshot(id int64) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
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

func (m *Manager) markProbe(id int64) {
	m.mu.Lock()
	m.get(id).lastProbe = time.Now()
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
