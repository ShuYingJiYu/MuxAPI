// Package health 管理渠道熔断、模型能力缓存、路由统计与状态告警。
package health

import (
	"sort"
	"sync"
	"time"
)

// State is the channel-level circuit breaker state.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
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

type breaker struct {
	state             State
	fails             int
	openUntil         time.Time
	halfOpenInFlight  bool
	halfOpenClaimAt   time.Time
	recoverySuccesses int
	reopenCount       int
	lastProbe         time.Time

	latencyMs   int64
	latencyEWMA float64
	inFlight    int64

	reqs       int64
	failReqs   int64
	totLatency int64
	latSamples int64
	trend      []TrendPoint
	lastReqs   int64
	lastFails  int64
}

type modelKey struct {
	upstreamID int64
	model      string
}

// TrendPoint is one channel health sample.
type TrendPoint struct {
	TS       int64   `json:"ts"`
	Status   int     `json:"status"`
	LatMs    int64   `json:"lat_ms"`
	SuccRate float64 `json:"succ_rate"`
}

const (
	statNoData   = 0
	statOK       = 1
	statDegraded = 2
	statDown     = 3
	trendCap     = 60
	ewmaAlpha    = 0.3

	defaultModelUnsupportedTTL = 5 * time.Minute
	defaultRecoverySuccessGoal = 2
	defaultMaxCooldown         = 5 * time.Minute
)

// AlertEvent reports a channel breaker transition. Model is the request model
// that triggered the transition, not a separate model-level breaker key.
type AlertEvent struct {
	UpstreamID int64  `json:"upstream_id"`
	Model      string `json:"model"`
	FromState  string `json:"from_state"`
	ToState    string `json:"to_state"`
	Fails      int    `json:"fails"`
	TS         int64  `json:"ts"`
}

type Alerter interface {
	Notify(ev AlertEvent)
}

// Manager owns one breaker per upstream. Model-specific state is limited to a
// short negative capability cache and never participates in channel recovery.
type Manager struct {
	mu            sync.Mutex
	breakers      map[int64]*breaker
	unsupported   map[modelKey]time.Time
	failThreshold int
	cooldown      time.Duration
	recoveryGoal  int
	maxCooldown   time.Duration
	modelTTL      time.Duration
	alerter       Alerter
}

// New 创建渠道级健康管理器，并规范化失败阈值与冷却时间。
func New(failThreshold int, cooldown time.Duration) *Manager {
	if failThreshold < 1 {
		failThreshold = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Manager{
		breakers:      make(map[int64]*breaker),
		unsupported:   make(map[modelKey]time.Time),
		failThreshold: failThreshold,
		cooldown:      cooldown,
		recoveryGoal:  defaultRecoverySuccessGoal,
		maxCooldown:   defaultMaxCooldown,
		modelTTL:      defaultModelUnsupportedTTL,
	}
}

// SetAdvancedPolicy updates recovery and capability-cache behavior without a
// restart. Invalid values fall back to conservative defaults.
func (m *Manager) SetAdvancedPolicy(recoveryGoal int, maxCooldown, modelTTL time.Duration) {
	if recoveryGoal < 1 {
		recoveryGoal = defaultRecoverySuccessGoal
	}
	if maxCooldown <= 0 {
		maxCooldown = defaultMaxCooldown
	}
	if modelTTL <= 0 {
		modelTTL = defaultModelUnsupportedTTL
	}
	m.mu.Lock()
	m.recoveryGoal, m.maxCooldown, m.modelTTL = recoveryGoal, maxCooldown, modelTTL
	m.mu.Unlock()
}

// SetAlerter 设置状态翻转通知器。
func (m *Manager) SetAlerter(a Alerter) { m.alerter = a }

// SetFailurePolicy updates the breaker policy for future state transitions.
// Existing OPEN channels keep their current deadline; the new cooldown is used
// when they are opened or reopened next.
func (m *Manager) SetFailurePolicy(failThreshold int, cooldown time.Duration) {
	if failThreshold < 1 {
		failThreshold = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	m.mu.Lock()
	m.failThreshold = failThreshold
	m.cooldown = cooldown
	m.mu.Unlock()
}

func (m *Manager) get(id int64) *breaker {
	b := m.breakers[id]
	if b == nil {
		b = &breaker{state: Closed}
		m.breakers[id] = b
	}
	return b
}

func (m *Manager) canServe(b *breaker) bool {
	now := time.Now()
	// 冷却到期只进入半开态，真正恢复仍需后续成功结果确认。
	if b.state == Open {
		if now.Before(b.openUntil) {
			return false
		}
		b.state = HalfOpen
		b.recoverySuccesses = 0
		b.halfOpenInFlight = false
	}
	if b.state == HalfOpen && b.halfOpenInFlight && now.Sub(b.halfOpenClaimAt) > m.cooldown {
		b.halfOpenInFlight = false
	}
	return b.state != HalfOpen || !b.halfOpenInFlight
}

func (m *Manager) modelUnsupportedLocked(id int64, model string) bool {
	if model == "" {
		return false
	}
	k := modelKey{id, model}
	expires, ok := m.unsupported[k]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(m.unsupported, k)
		return false
	}
	return true
}

// IsAvailable checks channel health and the model capability cache.
func (m *Manager) IsAvailable(id int64, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelUnsupportedLocked(id, model) {
		return false
	}
	return m.canServe(m.get(id))
}

// Claim reserves a request slot. HALF_OPEN permits exactly one concurrent
// request; CLOSED requests only increment the load counter used by P2C.
func (m *Manager) Claim(id int64, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelUnsupportedLocked(id, model) {
		return false
	}
	b := m.get(id)
	if !m.canServe(b) {
		return false
	}
	if b.state == HalfOpen {
		b.halfOpenInFlight = true
		b.halfOpenClaimAt = time.Now()
	}
	b.inFlight++
	return true
}

// ReleaseClaim releases the generic load counter and the HALF_OPEN gate.
func (m *Manager) ReleaseClaim(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	if b.inFlight > 0 {
		b.inFlight--
	}
	if b.state == HalfOpen {
		b.halfOpenInFlight = false
	}
}

func (m *Manager) InFlight(id int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(id).inFlight
}

// MarkModelUnsupported excludes a deterministic model/channel mismatch for a
// short period without changing channel health.
func (m *Manager) MarkModelUnsupported(id int64, model string) {
	if model == "" {
		return
	}
	m.mu.Lock()
	m.unsupported[modelKey{id, model}] = time.Now().Add(m.modelTTL)
	m.mu.Unlock()
}

func (m *Manager) MarkModelSupported(id int64, model string) {
	if model == "" {
		return
	}
	m.mu.Lock()
	delete(m.unsupported, modelKey{id, model})
	m.mu.Unlock()
}

func (m *Manager) MarkModelsSupported(id int64, models []string) {
	m.mu.Lock()
	for _, model := range models {
		delete(m.unsupported, modelKey{id, model})
	}
	m.mu.Unlock()
}

func (m *Manager) IsModelUnsupported(id int64, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modelUnsupportedLocked(id, model)
}

// LatencyEWMA 返回渠道的成功请求 TTFT EWMA(ms)，供 P2C 比较；0 表示冷启动。
func (m *Manager) LatencyEWMA(id int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(m.get(id).latencyEWMA)
}

// Report records one business attempt against the channel breaker.
func (m *Manager) Report(id int64, model string, ok bool, latencyMs int64) {
	m.mu.Lock()
	b := m.get(id)
	b.reqs++
	if !ok {
		b.failReqs++
	} else if latencyMs > 0 {
		b.totLatency += latencyMs
		b.latSamples++
		b.latencyMs = latencyMs
	}
	// A request that was already in flight before another request opened the
	// circuit is not a controlled recovery trial. Its late success may update
	// statistics, but must not bypass the configured cool-down.
	from, to := b.state, b.state
	if !(ok && b.state == Open) {
		from, to = m.drive(b, ok, latencyMs)
	}
	ev, flipped := transitionEvent(id, model, from, to, b.fails)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// ReportTimeout immediately opens a channel after a confirmed response stall.
// A timeout can hold a request slot for minutes, so waiting for the normal
// failure threshold would allow more requests to select the same channel.
func (m *Manager) ReportTimeout(id int64, model string, latencyMs int64) {
	m.mu.Lock()
	b := m.get(id)
	b.reqs++
	b.failReqs++
	if b.fails < m.failThreshold-1 {
		b.fails = m.failThreshold - 1
	}
	from, to := m.drive(b, false, latencyMs)
	ev, flipped := transitionEvent(id, model, from, to, b.fails)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// ObserveProbe uses the same channel state machine as business traffic but
// does not affect business request counters. Two successes are required to
// close an OPEN/HALF_OPEN channel.
func (m *Manager) ObserveProbe(id int64, model string, ok bool, latencyMs int64) {
	m.mu.Lock()
	b := m.get(id)
	b.lastProbe = time.Now()
	if ok && model != "" {
		delete(m.unsupported, modelKey{id, model})
	}
	from, to := m.drive(b, ok, latencyMs)
	ev, flipped := transitionEvent(id, model, from, to, b.fails)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// Forget 丢弃某渠道的全部内存状态，供上游被删除时调用。
// 不调用则 breakers/unsupported 会随删除累积，Sample() 还会继续为其追加趋势点。
func (m *Manager) Forget(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.breakers, id)
	for key := range m.unsupported {
		if key.upstreamID == id {
			delete(m.unsupported, key)
		}
	}
}

// ResetCircuit manually returns one channel to CLOSED without changing its
// traffic statistics or model capability cache.
func (m *Manager) ResetCircuit(id int64) {
	m.mu.Lock()
	b := m.get(id)
	from := b.state
	b.state = Closed
	b.fails = 0
	b.openUntil = time.Time{}
	b.halfOpenInFlight = false
	b.halfOpenClaimAt = time.Time{}
	b.recoverySuccesses = 0
	b.reopenCount = 0
	ev, flipped := transitionEvent(id, "", from, Closed, 0)
	m.mu.Unlock()
	if flipped {
		m.dispatch(ev)
	}
}

// drive 是业务请求与主动探测共用的熔断状态机。
func (m *Manager) drive(b *breaker, ok bool, latencyMs int64) (from, to State) {
	from = b.state
	if ok {
		b.fails = 0
		if latencyMs > 0 {
			b.latencyMs = latencyMs
			if b.latencyEWMA == 0 {
				b.latencyEWMA = float64(latencyMs)
			} else {
				b.latencyEWMA = ewmaAlpha*float64(latencyMs) + (1-ewmaAlpha)*b.latencyEWMA
			}
		}
		switch b.state {
		case Closed:
			b.recoverySuccesses = 0
		case Open:
			b.state = HalfOpen
			b.recoverySuccesses = 1
			b.halfOpenInFlight = false
		case HalfOpen:
			b.recoverySuccesses++
			b.halfOpenInFlight = false
			if b.recoverySuccesses >= m.recoveryGoal {
				b.state = Closed
				b.recoverySuccesses = 0
				b.reopenCount = 0
			}
		}
		return from, b.state
	}

	b.fails++
	b.recoverySuccesses = 0
	if b.state == HalfOpen || b.state == Open || b.fails >= m.failThreshold {
		b.state = Open
		b.halfOpenInFlight = false
		b.reopenCount++
		b.openUntil = time.Now().Add(m.backoff(b.reopenCount))
	}
	return from, b.state
}

// backoff 对反复熔断使用指数冷却，最长为五分钟或基础冷却时间。
func (m *Manager) backoff(reopenCount int) time.Duration {
	if reopenCount < 1 {
		return m.cooldown
	}
	shift := reopenCount - 1
	if shift > 6 {
		shift = 6
	}
	d := m.cooldown * time.Duration(1<<shift)
	capDuration := m.maxCooldown
	if m.cooldown > capDuration {
		capDuration = m.cooldown
	}
	if d > capDuration {
		return capDuration
	}
	return d
}

func (m *Manager) dispatch(ev AlertEvent) {
	if m.alerter != nil {
		go m.alerter.Notify(ev)
	}
}

func transitionEvent(id int64, model string, from, to State, fails int) (AlertEvent, bool) {
	flip := (from == Closed && to == Open) || (from != Closed && to == Closed)
	if !flip {
		return AlertEvent{}, false
	}
	return AlertEvent{
		UpstreamID: id,
		Model:      model,
		FromState:  from.String(),
		ToState:    to.String(),
		Fails:      fails,
		TS:         time.Now().Unix(),
	}, true
}

// RouteSample 是从历史审计恢复渠道延迟 EWMA 所需的最小数据。
type RouteSample struct {
	UpstreamID int64
	OK         bool
	LatencyMs  int64
}

// Seed restores channel-level routing statistics without restoring OPEN state.
func (m *Manager) Seed(samples []RouteSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sample := range samples {
		b := m.get(sample.UpstreamID)
		if sample.OK && sample.LatencyMs > 0 {
			if b.latencyEWMA == 0 {
				b.latencyEWMA = float64(sample.LatencyMs)
			} else {
				b.latencyEWMA = ewmaAlpha*float64(sample.LatencyMs) + (1-ewmaAlpha)*b.latencyEWMA
			}
		}
	}
}

// Snapshot 是管理接口读取的渠道级健康快照。
type Snapshot struct {
	State     string
	Fails     int
	LatencyMs int64
	OpenUntil time.Time
	LastProbe time.Time
	Reqs      int64
	FailReqs  int64
	SuccRate  float64
	AvgLatMs  int64
	InFlight  int64
	Trend     []TrendPoint
}

// Snapshot 返回指定渠道的累计统计与当前熔断状态副本。
func (m *Manager) Snapshot(id int64) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	sn := Snapshot{
		State: b.state.String(), Fails: b.fails, LatencyMs: b.latencyMs,
		OpenUntil: b.openUntil, LastProbe: b.lastProbe,
		Reqs: b.reqs, FailReqs: b.failReqs, InFlight: b.inFlight,
		Trend: append([]TrendPoint(nil), b.trend...),
	}
	if b.reqs > 0 {
		sn.SuccRate = float64(b.reqs-b.failReqs) / float64(b.reqs)
	}
	if b.latSamples > 0 {
		sn.AvgLatMs = b.totLatency / b.latSamples
	}
	return sn
}

// ModelHealth 表示一条模型能力排除记录，不是独立熔断器。
type ModelHealth struct {
	Model string `json:"model"`
	State string `json:"state"`
}

func (m *Manager) ModelStates(id int64) []ModelHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]ModelHealth, 0)
	for key, expires := range m.unsupported {
		if now.After(expires) {
			delete(m.unsupported, key)
			continue
		}
		if key.upstreamID == id {
			out = append(out, ModelHealth{Model: key.model, State: "UNSUPPORTED"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (m *Manager) EffectiveState(id int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(id).state.String()
}

// Sample 将累计请求计数转换成一个趋势采样点，并限制内存窗口长度。
func (m *Manager) Sample() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	for _, b := range m.breakers {
		dReqs := b.reqs - b.lastReqs
		dFails := b.failReqs - b.lastFails
		rate := 1.0
		if dReqs > 0 {
			rate = float64(dReqs-dFails) / float64(dReqs)
		}
		b.lastReqs, b.lastFails = b.reqs, b.failReqs
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
