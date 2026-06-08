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
	openUntil   time.Time // Open 冷却到期时间：到期后 canServe 翻 HalfOpen 放行恢复流量
	lastProbe   time.Time
	latencyMs   int64
	latencyEWMA float64 // 成功请求延迟的指数加权移动平均(ms)，供 P2C 选路；0=尚无数据
	succEWMA    float64 // 成功率的指数加权移动平均(0..1)：成功样本=1、失败=0。供延迟加权选路算「有效延迟」；
	// 用 EWMA 而非全历史比例——能快速反映「正在变差」，不被陈旧好成绩稀释。-1=尚无样本(新键乐观，视为 1)。
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
		b = &breaker{state: Closed, succEWMA: -1} // succEWMA=-1：尚无样本，RouteStats 据此乐观视为 1
		m.breakers[k] = b
	}
	return b
}

// canServe 只读判定：该键现在能否承接业务请求（调用方须持有 m.mu）。
//
//   - Closed：可服务。
//   - Open 未到冷却：不可服务（死渠道不吃业务流量）。
//   - Open 冷却到期：翻 HalfOpen → 可服务（恢复期放行业务流量自证）。
//   - HalfOpen：可服务（不限并发）。半开期放行所有请求去试这个上游——
//     健康渠道刚恢复时并发能立即满血；真死渠道则被一波请求各打一次、
//     drive 对半开态失败立即重新 Open，死渠道保护仍在。
//
// 不再用「半开单请求闸门」：在严格优先级(主备)拓扑下，单闸门会把主力上游
// 恢复期的并发请求拦成不可用→选路 no upstream available→503，且备份失败转移
// 也被选路阶段短路。放行让恢复瞬间不丢流量、失败也能正常下沉备份。
func (m *Manager) canServe(b *breaker) bool {
	if b.state == Open {
		if !time.Now().After(b.openUntil) {
			return false
		}
		b.state = HalfOpen // 冷却结束，进入恢复期，放行业务流量自证
	}
	return true
}

// IsAvailable 调度层询问：该上游的该模型现在能用吗？
// 双层判定：上游级(凭证类故障)与模型级须都能服务。
// canServe 在 Open 冷却到期时会把状态翻 HalfOpen(放行恢复流量)，故非纯只读，
// 但无并发名额占用——半开期所有候选都可用，不再有「单试探闸门」。
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
	return true
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

// RouteStats 返回该 (上游,模型) 的「成功延迟 EWMA(ms) + 成功率(0..1)」，供调度层算有效延迟做延迟加权选路。
// 模型级键优先(与选路粒度一致)、该粒度无数据时回退上游级键。
// 约定：ewmaMs=0 表示延迟未知(冷启动)，调度层据此给新渠道探数据的机会；
// succRate 在 succEWMA=-1(无样本)时乐观返回 1，避免新渠道一上来就被判低成功率压死。
func (m *Manager) RouteStats(id int64, model string) (ewmaMs float64, succRate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pick := func(b *breaker) (float64, float64, bool) {
		if b == nil || (b.latencyEWMA == 0 && b.succEWMA < 0) {
			return 0, 1, false // 该键完全无数据
		}
		sr := b.succEWMA
		if sr < 0 {
			sr = 1 // 有延迟样本但无成功率样本(理论少见)：乐观
		}
		return b.latencyEWMA, sr, true
	}
	if model != "" {
		if lat, sr, ok := pick(m.breakers[breakerKey{id, model}]); ok {
			return lat, sr
		}
	}
	if lat, sr, ok := pick(m.breakers[breakerKey{id, ""}]); ok {
		return lat, sr
	}
	return 0, 1 // 全无数据：冷启动，延迟未知 + 成功率乐观
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
// scope 完全由调用方(prober)按口径决定，本方法忠实驱动该 scope，不再额外连带：
//   - 凭证类失败(401/402/403)→ scope="" 熔整上游；
//   - 渠道级探测(上游开关开)成功 → scope="" 复活整渠道；
//   - 其余 → scope=model 仅作用该模型。
//
// 连带策略集中在 prober.observe，本方法保持单一职责（见 [[probe-scope-by-channel-switch]]）。
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
// 半开态(HalfOpen)只要失败一次即重新 Open——这是死渠道保护：半开期放行的并发
// 业务流量若打到真死渠道，每个失败回执都会立即把它打回 Open，重新进入冷却。
func (m *Manager) drive(b *breaker, ok bool, latencyMs int64) (from, to State) {
	from = b.state
	b.succEWMA = mixEWMA(b.succEWMA, boolToF(ok)) // 成功率 EWMA：成功=1 失败=0，首样本直赋(见 mixEWMA)
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

// mixEWMA 指数加权移动平均：哨兵 prev<0 表示「尚无样本」，直接取新值(避免被 0 拖低)；
// 否则按 ewmaAlpha 混合。供 succEWMA 用(成功率范围 0..1，-1 作无样本哨兵)。
func mixEWMA(prev, sample float64) float64 {
	if prev < 0 {
		return sample
	}
	return ewmaAlpha*sample + (1-ewmaAlpha)*prev
}

func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// RouteSample 一条历史路由样本，用于 Seed 回放重建 EWMA（与 store.RouteSample 同构，避免跨包依赖）。
type RouteSample struct {
	UpstreamID int64
	Model      string
	OK         bool
	LatencyMs  int64
}

// Seed 启动时用历史样本【按正序回放】重建各 (上游,模型) 的延迟/成功率 EWMA，
// 让「流量分配预估」在重启后不归零。只重建选路所需的统计(latencyEWMA/succEWMA)，
// 【不重建熔断状态】——重启后一律从 Closed 起，开关交给重启后的新流量决定，
// 不把历史故障态强加给新世界(那会导致重启即误熔断)。上游级键同步累积，供看板与上游级选路回退。
func (m *Manager) Seed(samples []RouteSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	apply := func(b *breaker, s RouteSample) {
		b.succEWMA = mixEWMA(b.succEWMA, boolToF(s.OK))
		if s.OK && s.LatencyMs > 0 { // 仅成功请求计入延迟 EWMA（与 drive 一致）
			if b.latencyEWMA == 0 {
				b.latencyEWMA = float64(s.LatencyMs)
			} else {
				b.latencyEWMA = ewmaAlpha*float64(s.LatencyMs) + (1-ewmaAlpha)*b.latencyEWMA
			}
		}
	}
	for _, s := range samples {
		apply(m.get(breakerKey{s.UpstreamID, ""}), s) // 上游级
		if s.Model != "" {
			apply(m.get(breakerKey{s.UpstreamID, s.Model}), s) // 模型级
		}
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
	Model     string  `json:"model"`      // 模型名
	State     string  `json:"state"`      // CLOSED 正常 / OPEN 熔断 / HALF_OPEN 半开
	Fails     int     `json:"fails"`      // 当前连续失败数
	LatencyMs int64   `json:"latency_ms"` // 最近一次延迟(ms)
	LastProbe int64   `json:"last_probe"` // 最后探测 unix 秒，0=从未探测
	LatEWMA   float64 `json:"lat_ewma"`   // 成功延迟 EWMA(ms)，供智能路由展示；0=未知
	SuccEWMA  float64 `json:"succ_ewma"`  // 成功率 EWMA(0..1)，供智能路由展示；无样本归一为 1
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
		succ := b.succEWMA
		if succ < 0 {
			succ = 1 // 无样本：智能路由展示口径乐观为 1（与 RouteStats 一致）
		}
		out = append(out, ModelHealth{
			Model: k.model, State: b.state.String(), Fails: b.fails,
			LatencyMs: b.latencyMs, LastProbe: lastProbe,
			LatEWMA: b.latencyEWMA, SuccEWMA: succ,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// EffectiveState 返回该上游「对外真实运行状态」，供看板展示与「生效层」判定使用。
// 纯只读：不调 canServe，不触发 OPEN→HalfOpen 翻转，仅快照当前状态。
//
// 规则（修复「单模型上游唯一探测失败却仍显正常」）：
//   - 上游级(model="")OPEN → 整体 OPEN（凭证/余额类连坐所有模型）；
//   - 否则看模型级键：任一可用(CLOSED/HALF_OPEN)则整体可用，全部 OPEN → 整体 OPEN；
//   - 无任何模型级键（从未按模型探过）→ 回退上游级 State（保留「待探测/正常」原样）。
func (m *Manager) EffectiveState(id int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if up := m.breakers[breakerKey{id, ""}]; up != nil && up.state == Open {
		return Open.String() // 上游级熔断：所有模型连坐
	}
	// 模型级聚合：anyClosed 优先(正常) > anyHalf(半开) > 全 OPEN(熔断)。
	// 注意 State 枚举值 Closed=0/Open=1/HalfOpen=2 并非按健康度排序，不能直接比大小。
	hasModel, anyClosed, anyHalf := false, false, false
	for k, b := range m.breakers {
		if k.upstreamID != id || k.model == "" {
			continue
		}
		hasModel = true
		switch b.state {
		case Closed:
			anyClosed = true
		case HalfOpen:
			anyHalf = true
		}
	}
	if !hasModel {
		return m.snapshotState(id) // 从未按模型探过：照旧用上游级状态
	}
	switch {
	case anyClosed:
		return Closed.String()
	case anyHalf:
		return HalfOpen.String()
	default:
		return Open.String() // 所有模型都 OPEN → 整个上游熔断（oojj 单模型死掉即此情形）
	}
}

// snapshotState 只读取上游级键的状态字符串（无键＝从未探测，按 Closed 处理）。
func (m *Manager) snapshotState(id int64) string {
	if up := m.breakers[breakerKey{id, ""}]; up != nil {
		return up.state.String()
	}
	return Closed.String()
}

func (m *Manager) markProbe(id int64, model string) {
	m.mu.Lock()
	now := time.Now()
	m.get(breakerKey{id, model}).lastProbe = now
	if model != "" {
		// 上游级键(model="")是看板「上游池/成员行」运行时列的数据源；它本身从不被直接探测，
		// 但「该上游下任一模型被探测过」在上游级聚合视图里即成立——同步它的 lastProbe，
		// 否则上游池行会因 lastProbe==0 永久显示「待探测」(即使模型徽章已探到、显示正常)。
		m.get(breakerKey{id, ""}).lastProbe = now
	}
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
