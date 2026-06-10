package health

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerStateMachine(t *testing.T) {
	m := New(3, 50*time.Millisecond)
	const id = int64(1)

	// 初始 CLOSED 可用
	if !m.IsAvailable(id, "") || m.Snapshot(id).State != "CLOSED" {
		t.Fatal("初始应 CLOSED 可用")
	}
	// 失败未达阈值，仍 CLOSED
	m.Report(id, "", false, 0)
	m.Report(id, "", false, 0)
	if !m.IsAvailable(id, "") {
		t.Fatal("2次失败<阈值3，应仍可用")
	}
	// 第3次失败 → OPEN，不可用
	m.Report(id, "", false, 0)
	if m.IsAvailable(id, "") || m.Snapshot(id).State != "OPEN" {
		t.Fatal("达阈值应 OPEN 不可用")
	}
	// 冷却到期 → IsAvailable 触发 HALF_OPEN（可用，放试探）
	time.Sleep(60 * time.Millisecond)
	if !m.IsAvailable(id, "") || m.Snapshot(id).State != "HALF_OPEN" {
		t.Fatalf("冷却后应 HALF_OPEN，实际 %s", m.Snapshot(id).State)
	}
	// 半开期失败 → 立即重新 OPEN
	m.Report(id, "", false, 0)
	if m.IsAvailable(id, "") {
		t.Fatal("半开失败应立即 OPEN")
	}
	// 冷却后半开 + 成功 → CLOSED 恢复
	time.Sleep(60 * time.Millisecond)
	m.IsAvailable(id, "") // → HALF_OPEN
	m.Report(id, "", true, 120)
	s := m.Snapshot(id)
	if s.State != "CLOSED" || s.Fails != 0 || s.LatencyMs != 120 {
		t.Fatalf("成功应 CLOSED 重置，实际 %+v", s)
	}
}

// 统计：Report 计入业务流量，reportProbe(探测) 不计入。
func TestTrafficStats(t *testing.T) {
	m := New(100, time.Second) // 高阈值避免熔断干扰
	const id = int64(7)

	m.Report(id, "", true, 100)      // 成功 100ms
	m.Report(id, "", true, 200)      // 成功 200ms
	m.Report(id, "", false, 0)       // 失败
	m.reportProbe(id, "", true, 999) // 探测：不应计入 reqs/延迟
	m.reportProbe(id, "", false, 0)  // 探测失败：不应计入

	s := m.Snapshot(id)
	if s.Reqs != 3 {
		t.Fatalf("应只计 3 次业务请求(探测不算)，实际 %d", s.Reqs)
	}
	if s.FailReqs != 1 {
		t.Fatalf("应 1 次失败，实际 %d", s.FailReqs)
	}
	if s.SuccRate < 0.66 || s.SuccRate > 0.67 {
		t.Fatalf("成功率应 ≈0.667，实际 %f", s.SuccRate)
	}
	if s.AvgLatMs != 150 { // (100+200)/2 成功请求平均
		t.Fatalf("平均延迟应 150ms，实际 %d", s.AvgLatMs)
	}

	// 无请求的上游：统计全 0，不 panic
	if z := m.Snapshot(999); z.Reqs != 0 || z.SuccRate != 0 || z.AvgLatMs != 0 {
		t.Fatalf("空上游统计应全 0，实际 %+v", z)
	}
}

// 验证 Sample 用窗口增量算成功率，且环形缓冲不超过容量。
func TestSampleTrend(t *testing.T) {
	m := New(100, time.Second)
	const id = int64(3)

	// 窗口1：2 成功 1 失败 → 成功率 2/3
	m.Report(id, "", true, 100)
	m.Report(id, "", true, 100)
	m.Report(id, "", false, 0)
	m.Sample()
	// 窗口2：1 成功 1 失败 → 成功率 0.5（只看本窗口增量，不含上一窗口）
	m.Report(id, "", true, 100)
	m.Report(id, "", false, 0)
	m.Sample()

	tr := m.Snapshot(id).Trend
	if len(tr) != 2 {
		t.Fatalf("应有 2 个采样点，实际 %d", len(tr))
	}
	if r := tr[0].SuccRate; r < 0.66 || r > 0.67 {
		t.Fatalf("窗口1成功率应 ≈0.667，实际 %f", r)
	}
	if tr[1].SuccRate != 0.5 {
		t.Fatalf("窗口2成功率应 0.5（仅本窗口增量），实际 %f", tr[1].SuccRate)
	}
	// 两个窗口都有失败 → 状态应为降级（橙）
	if tr[0].Status != statDegraded || tr[1].Status != statDegraded {
		t.Fatalf("有失败的窗口状态应为降级(%d)，实际 %d/%d", statDegraded, tr[0].Status, tr[1].Status)
	}

	// 熔断中的上游：栅栏状态应为 down（红）
	down := New(1, time.Minute) // 阈值1：一次失败即熔断
	down.Report(9, "", false, 0)
	down.Sample()
	if s := down.Snapshot(9).Trend[0].Status; s != statDown {
		t.Fatalf("熔断上游状态应为 down(%d)，实际 %d", statDown, s)
	}

	// 全程无流量、从未探测的上游：状态应为无数据（灰），不能误判成健康
	idle := New(3, time.Minute)
	idle.IsAvailable(5, "") // 仅注册，不产生流量也不探测
	idle.Sample()
	if s := idle.Snapshot(5).Trend[0].Status; s != statNoData {
		t.Fatalf("空闲未探测上游状态应为无数据(%d)，实际 %d", statNoData, s)
	}

	// 环形缓冲不越界
	for i := 0; i < trendCap+10; i++ {
		m.Sample()
	}
	if got := len(m.Snapshot(id).Trend); got != trendCap {
		t.Fatalf("环形缓冲应封顶 %d，实际 %d", trendCap, got)
	}
}

// 核心：模型级熔断不波及同上游的其他模型。
func TestModelLevelIsolation(t *testing.T) {
	m := New(1, time.Minute) // 一次失败即熔断
	const id = int64(1)

	// modelA 失败 → 仅 (id, A) 熔断
	m.Report(id, "modelA", false, 0)
	if m.IsAvailable(id, "modelA") {
		t.Fatal("modelA 失败应熔断 modelA")
	}
	if !m.IsAvailable(id, "modelB") {
		t.Fatal("modelA 熔断不应波及 modelB ← 核心痛点：gpt-5.5 挂不连累 claude")
	}
	// 不带 model 的上游级判定也应可用（上游本身没坏）
	if !m.IsAvailable(id, "") {
		t.Fatal("仅模型级熔断，上游级应仍可用")
	}
}

// 凭证类失败(401/402/403→model="")熔断整个上游，所有模型连坐。
func TestUpstreamLevelCutoff(t *testing.T) {
	m := New(1, time.Minute)
	const id = int64(2)

	// 模拟 forward 对 401 的处理：scope="" → 上游级熔断
	m.Report(id, "", false, 0)
	if m.IsAvailable(id, "anyModel") {
		t.Fatal("上游级熔断应连坐所有模型")
	}
	if m.IsAvailable(id, "") {
		t.Fatal("上游级熔断后上游级判定也应不可用")
	}
}

// model="" 回退路径与改造前等价：另一上游不受影响、Snapshot 按上游聚合。
func TestUpstreamLevelEquivalence(t *testing.T) {
	m := New(1, time.Minute)
	m.Report(1, "", false, 0) // 上游 1 上游级熔断
	if m.IsAvailable(1, "") {
		t.Fatal("上游1应熔断")
	}
	if !m.IsAvailable(2, "") {
		t.Fatal("上游2不应受上游1影响")
	}
	// 流量统计仍按上游聚合（看板不变）
	if s := m.Snapshot(1); s.Reqs != 1 || s.FailReqs != 1 {
		t.Fatalf("Snapshot 应按上游聚合统计，实际 %+v", s)
	}
}

// 模型级失败的流量统计仍累计到上游级 Snapshot（看板按上游聚合，不漏计）。
func TestModelFailCountsToUpstreamSnapshot(t *testing.T) {
	m := New(100, time.Minute) // 高阈值避免熔断干扰
	const id = int64(3)
	m.Report(id, "modelA", true, 100)
	m.Report(id, "modelA", false, 0)
	m.Report(id, "modelB", true, 200)
	s := m.Snapshot(id)
	if s.Reqs != 3 || s.FailReqs != 1 {
		t.Fatalf("上游级 Snapshot 应聚合所有模型流量，实际 reqs=%d fail=%d", s.Reqs, s.FailReqs)
	}
}

// EWMA：首次直接赋值；后续按 α=0.3 平滑；失败不计入；新趋势能被逐步跟上。
func TestLatencyEWMA(t *testing.T) {
	m := New(100, time.Minute) // 高阈值避免熔断干扰
	const id = int64(11)
	const model = "m"

	// 尚无数据 → 未知(0)
	if e := m.LatencyEWMA(id, model); e != 0 {
		t.Fatalf("无数据应返回 0，实际 %d", e)
	}

	// 首次成功 100ms → EWMA 直接赋值 100
	m.Report(id, model, true, 100)
	if e := m.LatencyEWMA(id, model); e != 100 {
		t.Fatalf("首次应直接赋值 100，实际 %d", e)
	}

	// 第二次 200ms → 0.3*200 + 0.7*100 = 130
	m.Report(id, model, true, 200)
	if e := m.LatencyEWMA(id, model); e != 130 {
		t.Fatalf("EWMA 应为 130，实际 %d", e)
	}

	// 失败请求不计入 EWMA（延迟无意义），应仍是 130
	m.Report(id, model, false, 0)
	if e := m.LatencyEWMA(id, model); e != 130 {
		t.Fatalf("失败不应改变 EWMA，应仍 130，实际 %d", e)
	}

	// 持续灌入低延迟 10ms，EWMA 应逐步衰减贴近 10（验证能跟上新趋势）
	for i := 0; i < 30; i++ {
		m.Report(id, model, true, 10)
	}
	if e := m.LatencyEWMA(id, model); e > 15 {
		t.Fatalf("持续低延迟后 EWMA 应衰减贴近 10，实际 %d", e)
	}
}

// LatencyEWMA 回退：模型级无数据时回退上游级键。
func TestLatencyEWMAFallback(t *testing.T) {
	m := New(100, time.Minute)
	const id = int64(12)

	// 只喂上游级(model="")延迟
	m.Report(id, "", true, 80)
	// 查询某个从未上报过的模型 → 回退上游级 80
	if e := m.LatencyEWMA(id, "neverSeen"); e != 80 {
		t.Fatalf("模型级无数据应回退上游级 80，实际 %d", e)
	}
}

// ModelStates：只返回模型级(model!="")键、按模型名排序稳定、空上游返回空切片。
func TestModelStates(t *testing.T) {
	m := New(1, time.Minute) // 一次失败即熔断
	const id = int64(20)

	// 上游级键(model="")不应出现在结果里
	m.Report(id, "", true, 50)
	// modelB 成功一次（CLOSED，延迟 120）
	m.Report(id, "modelB", true, 120)
	// modelA 失败一次（阈值 1 → OPEN，fails=1）
	m.Report(id, "modelA", false, 0)
	// modelC 探测一次（标记 lastProbe）
	m.markProbe(id, "modelC")

	got := m.ModelStates(id)
	if len(got) != 3 {
		t.Fatalf("应有 3 个模型级键(上游级不计)，实际 %d：%+v", len(got), got)
	}
	// 排序稳定：A < B < C
	if got[0].Model != "modelA" || got[1].Model != "modelB" || got[2].Model != "modelC" {
		t.Fatalf("应按模型名排序 A/B/C，实际 %s/%s/%s", got[0].Model, got[1].Model, got[2].Model)
	}
	// 各状态正确
	if got[0].State != "OPEN" || got[0].Fails != 1 {
		t.Fatalf("modelA 应 OPEN fails=1，实际 %+v", got[0])
	}
	if got[1].State != "CLOSED" || got[1].LatencyMs != 120 {
		t.Fatalf("modelB 应 CLOSED 延迟 120，实际 %+v", got[1])
	}
	if got[2].LastProbe == 0 {
		t.Fatalf("modelC 应有探测时间戳，实际 %+v", got[2])
	}

	// 空上游：返回空切片（非 nil 误用也允许，len 为 0 即可）
	if z := m.ModelStates(999); len(z) != 0 {
		t.Fatalf("空上游应返回空，实际 %+v", z)
	}
}

// HalfOpen only allows one business probe; other requests should skip this upstream.
func TestHalfOpenAllowsSingleProbe(t *testing.T) {
	m := New(1, 30*time.Millisecond)
	const id = int64(30)

	m.Report(id, "", false, 0)
	if m.IsAvailable(id, "") {
		t.Fatal("open breaker should be unavailable before cooldown")
	}
	time.Sleep(40 * time.Millisecond)

	if !m.Claim(id, "") {
		t.Fatal("first half-open probe should be claimed")
	}
	if m.Snapshot(id).State != "HALF_OPEN" {
		t.Fatal("state should be HALF_OPEN after first probe")
	}
	if m.Claim(id, "") {
		t.Fatal("second half-open probe should be blocked while first is in flight")
	}

	m.Report(id, "", false, 0)
	if m.IsAvailable(id, "") {
		t.Fatal("failed half-open probe should reopen breaker")
	}
}

func TestHalfOpenConcurrentBurstAllowsOne(t *testing.T) {
	m := New(1, 30*time.Millisecond)
	const id = int64(32)
	m.Report(id, "", false, 0)
	time.Sleep(40 * time.Millisecond)
	var served, blocked int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.Claim(id, "") {
				atomic.AddInt64(&served, 1)
			} else {
				atomic.AddInt64(&blocked, 1)
			}
		}()
	}
	wg.Wait()
	if served != 1 || blocked != 7 {
		t.Fatalf("half-open burst should allow 1 and block 7, got served=%d blocked=%d", served, blocked)
	}
}

// 探测是死渠道的恢复主路径：业务流量在 Open 期间打不进去(IsAvailable=false)，
// 但探测器 ObserveProbe 不经 IsAvailable，探成功即把 Open/HalfOpen 翻回 Closed。
func TestProbeRecoversDeadUpstream(t *testing.T) {
	m := New(1, time.Hour) // 冷却极长：业务流量在冷却内永远试探不到
	const id = int64(31)
	const model = "gpt-5.4"

	m.Report(id, model, false, 0) // → 模型级 OPEN
	if m.IsAvailable(id, model) {
		t.Fatal("熔断后业务流量应打不进（冷却极长，业务无法自行试探）")
	}
	// 探测成功 → 立即恢复 Closed，无需等冷却
	m.ObserveProbe(id, model, true, 120)
	if !m.IsAvailable(id, model) {
		t.Fatal("探测成功应立即恢复可用（探测是恢复主路径，不受业务冷却限制）")
	}
	if s := m.ModelStates(id); len(s) != 1 || s[0].State != "CLOSED" {
		t.Fatalf("探测恢复后该模型应 CLOSED，实际 %+v", s)
	}
}

// Seed 回放历史样本重建选路预估：恢复 latency/succ EWMA，但不触碰熔断状态(保持 Closed)。
func TestSeedRebuildsRouteStats(t *testing.T) {
	m := New(3, time.Minute)
	const id, model = int64(7), "gpt-5.5"
	// 回放：3 次成功(延迟100/120/110) + 1 次失败
	m.Seed([]RouteSample{
		{UpstreamID: id, Model: model, OK: true, LatencyMs: 100},
		{UpstreamID: id, Model: model, OK: true, LatencyMs: 120},
		{UpstreamID: id, Model: model, OK: false, LatencyMs: 0},
		{UpstreamID: id, Model: model, OK: true, LatencyMs: 110},
	})
	// 模型级应重建出非零延迟 EWMA 与 <1 的成功率
	lat, sr := m.RouteStats(id, model)
	if lat <= 0 {
		t.Fatalf("Seed 后延迟 EWMA 应 >0，实际 %v", lat)
	}
	if sr <= 0 || sr >= 1 {
		t.Fatalf("有失败样本，成功率应在 (0,1)，实际 %v", sr)
	}
	// 关键：不重建熔断状态——重启后从 Closed 起，仍可用
	if !m.IsAvailable(id, model) {
		t.Fatal("Seed 不应改变熔断状态，应保持可用(Closed)")
	}
	if st := m.Snapshot(id); st.State != "CLOSED" {
		t.Fatalf("Seed 后上游级应 CLOSED，实际 %s", st.State)
	}
}

// 渠道级复活(breaker 契约)：探测以 scope=""(上游级)上报成功时，应复活整渠道——
// 被上游级故障连坐熔断的模型随之恢复；但被【自身故障】熔断的模型仍 OPEN，不被连带复活。
// prober 在「上游开了渠道级探测且本次成功」时就传 scope=""，故这里直接以 scope="" 验证 breaker 契约。

func TestProbeSuccessRevivesChannelLevel(t *testing.T) {
	m := New(1, time.Hour)
	const id = int64(41)

	m.Report(id, "", false, 0)
	if m.IsAvailable(id, "gpt-5.4") || m.IsAvailable(id, "gpt-5.5") {
		t.Fatal("upstream-level open should block all models")
	}

	m.ObserveProbe(id, "", true, 120)
	if !m.IsAvailable(id, "gpt-5.4") || !m.IsAvailable(id, "gpt-5.5") {
		t.Fatal("channel-level probe success should revive the channel")
	}

	m.Report(id, "gpt-5.4", false, 0)
	m.Report(id, "gpt-5.5", false, 0)
	if m.IsAvailable(id, "gpt-5.4") || m.IsAvailable(id, "gpt-5.5") {
		t.Fatal("model-level open should block the model")
	}

	m.ObserveProbe(id, "", true, 120)
	if !m.IsAvailable(id, "gpt-5.4") || !m.IsAvailable(id, "gpt-5.5") {
		t.Fatal("channel-level probe success should clear model-level opens")
	}
	for _, st := range m.ModelStates(id) {
		if st.State != "CLOSED" {
			t.Fatalf("all model states should be CLOSED after channel revive, got %+v", m.ModelStates(id))
		}
	}
}

func TestProbeSuccessModelLevelOnly(t *testing.T) {
	m := New(1, time.Hour)
	const id = int64(43)
	// 两个模型各自被自身故障熔断
	m.Report(id, "gpt-5.4", false, 0)
	m.Report(id, "gpt-5.5", false, 0)
	// 只探 gpt-5.4 成功(scope=model) → 只复活 gpt-5.4
	m.ObserveProbe(id, "gpt-5.4", true, 100)
	if !m.IsAvailable(id, "gpt-5.4") {
		t.Fatal("模型级探测成功应复活 gpt-5.4")
	}
	if m.IsAvailable(id, "gpt-5.5") {
		t.Fatal("模型级探测(scope=gpt-5.4)不应连带复活 gpt-5.5")
	}
}

// 探测【失败】不连带：探 gpt-5.4 失败(模型级)只熔 gpt-5.4，不连坐 gpt-5.5，
// 也不影响上游级（失败的连坐范围由调用方 scope 精确表达，本次成功复活逻辑不反向作恶）。
func TestProbeFailDoesNotCascade(t *testing.T) {
	m := New(1, time.Hour)
	const id = int64(42)
	m.ObserveProbe(id, "gpt-5.4", false, 0) // 模型级失败
	if m.IsAvailable(id, "gpt-5.4") {
		t.Fatal("探测失败的 gpt-5.4 应熔断")
	}
	if !m.IsAvailable(id, "gpt-5.5") {
		t.Fatal("探 gpt-5.4 失败(模型级)不应连坐 gpt-5.5")
	}
	if !m.IsAvailable(id, "") {
		t.Fatal("模型级探测失败不应熔断上游级")
	}
}

// EffectiveState 聚合：单模型上游唯一模型探测失败熔断 → 整体应 OPEN。
// 这是「oojj 只配一个探测、探测报错却仍显正常」bug 的回归用例。
func TestEffectiveStateSingleModelDown(t *testing.T) {
	m := New(1, time.Hour)
	const id = int64(50)
	m.ObserveProbe(id, "gpt-5.5", false, 0) // 唯一模型探测失败 → 模型级 OPEN
	if got := m.EffectiveState(id); got != "OPEN" {
		t.Fatalf("唯一模型熔断，整体应 OPEN，得到 %q", got)
	}
}

// EffectiveState 聚合：多模型上游只挂一个，其余正常 → 整体仍可用(CLOSED)。
func TestEffectiveStateOneOfManyDown(t *testing.T) {
	m := New(1, time.Hour)
	const id = int64(51)
	m.ObserveProbe(id, "gpt-5.4", false, 0)  // 一个挂
	m.ObserveProbe(id, "gpt-5.5", true, 100) // 一个好
	if got := m.EffectiveState(id); got != "CLOSED" {
		t.Fatalf("尚有可用模型，整体应 CLOSED，得到 %q", got)
	}
}

// EffectiveState 聚合：上游级(凭证类)熔断 → 整体 OPEN，连坐所有模型。
func TestEffectiveStateUpstreamLevelOpen(t *testing.T) {
	m := New(1, time.Hour)
	const id = int64(52)
	m.ObserveProbe(id, "gpt-5.5", true, 100) // 模型本身正常
	m.ObserveProbe(id, "", false, 0)         // 上游级失败(如 401)
	if got := m.EffectiveState(id); got != "OPEN" {
		t.Fatalf("上游级熔断应连坐整体 OPEN，得到 %q", got)
	}
}

// EffectiveState 聚合：从未按模型探过 → 回退上游级状态(默认 CLOSED，即「待探测」由前端再细分)。
func TestEffectiveStateNeverProbed(t *testing.T) {
	m := New(1, time.Hour)
	if got := m.EffectiveState(99); got != "CLOSED" {
		t.Fatalf("从未探测应回退 CLOSED，得到 %q", got)
	}
}

// 探测与业务流量阈值解耦：failThreshold=3 时，
// 业务流量(Report)失败一次仍 CLOSED（须连续3次才熔断，防偶发抖动误熔）；
// 而主动探测(ObserveProbe)失败一次即 OPEN（确定性健康信号，立即熔断）。
// 这是「看板/总览已红，分组/上游页却迟迟显示正常」bug 的回归用例。
func TestProbeFailsFastVsTraffic(t *testing.T) {
	m := New(3, time.Hour) // 生产默认阈值 3
	const id = int64(60)

	// 业务流量失败一次：未达阈值3，仍 CLOSED
	m.Report(id, "gpt-5.5", false, 0)
	if got := m.EffectiveState(id); got != "CLOSED" {
		t.Fatalf("业务流量失败1次<阈值3，应仍 CLOSED，得到 %q", got)
	}

	// 主动探测失败一次：立即 OPEN（不等阈值）
	m.ObserveProbe(id, "gpt-5.5", false, 0)
	if got := m.EffectiveState(id); got != "OPEN" {
		t.Fatalf("探测失败1次应立即 OPEN，得到 %q", got)
	}

	// 探测成功一次：立即恢复 CLOSED
	m.ObserveProbe(id, "gpt-5.5", true, 80)
	if got := m.EffectiveState(id); got != "CLOSED" {
		t.Fatalf("探测成功应立即恢复 CLOSED，得到 %q", got)
	}
}

// L1 回归：上游级键处于 HALF_OPEN 时，EffectiveState 不应被模型级聚合吞成 CLOSED，
// 须返回 HALF_OPEN，与无模型键路径(snapshotState)口径一致。
func TestEffectiveStateUpstreamHalfOpen(t *testing.T) {
	m := New(1, 20*time.Millisecond) // 阈值1：一次失败即熔断；短冷却便于翻 HALF_OPEN
	const id = int64(70)

	// 上游级失败 → OPEN；同时给一个正常模型级键，制造「模型聚合会算出 CLOSED」的陷阱
	m.Report(id, "", false, 0)
	m.Report(id, "gpt-5.5", true, 100)
	if got := m.EffectiveState(id); got != "OPEN" {
		t.Fatalf("上游级熔断应 OPEN，得到 %q", got)
	}

	// 冷却到期后触发翻 HALF_OPEN（IsAvailable 对上游级键 canServe 会翻态）
	time.Sleep(30 * time.Millisecond)
	if !m.IsAvailable(id, "") {
		t.Fatal("冷却到期上游级应可用(HALF_OPEN)")
	}
	// 此刻上游级=HALF_OPEN、模型级=CLOSED：必须返回 HALF_OPEN，不能被聚合成 CLOSED
	if got := m.EffectiveState(id); got != "HALF_OPEN" {
		t.Fatalf("上游级 HALF_OPEN 不应被模型聚合吞成 CLOSED，得到 %q", got)
	}
}

// L2 回归：model!="" 的 Report 须同步更新上游级键的 succEWMA/latencyEWMA，
// 使无模型键的回退路由(RouteStats/LatencyEWMA)读到鲜活值而非 Seed 冻结值。
func TestReportUpdatesUpstreamEWMA(t *testing.T) {
	m := New(100, time.Hour) // 高阈值避免熔断干扰
	const id = int64(71)

	// 仅按模型级上报成功流量
	m.Report(id, "gpt-5.5", true, 200)
	m.Report(id, "gpt-5.5", true, 200)

	// 上游级回退查询应能读到非零延迟 EWMA（修复前恒为 0/Seed 冻结）
	if lat := m.LatencyEWMA(id, ""); lat <= 0 {
		t.Fatalf("上游级 latencyEWMA 应被模型级流量同步更新为正值，得到 %d", lat)
	}
	ewmaMs, succ := m.RouteStats(id, "")
	if ewmaMs <= 0 {
		t.Fatalf("上游级回退 RouteStats 延迟应>0，得到 %f", ewmaMs)
	}
	if succ <= 0.99 { // 两次全成功，succEWMA 应趋近 1
		t.Fatalf("上游级回退成功率应≈1，得到 %f", succ)
	}

	// 反例：上游级键的状态/失败数绝不能被模型级流量带动
	if s := m.Snapshot(id); s.State != "CLOSED" || s.Fails != 0 {
		t.Fatalf("同步 EWMA 不应改动上游级 state/fails，得到 %+v", s)
	}
}

// L3 回归：平均延迟分子分母同口径——latencyMs==0 的成功样本既不进分子也不进分母，
// 不再系统性低估平均延迟。
func TestAvgLatencySameDenominator(t *testing.T) {
	m := New(100, time.Hour)
	const id = int64(72)

	m.Report(id, "", true, 100) // 计入：100ms
	m.Report(id, "", true, 200) // 计入：200ms
	m.Report(id, "", true, 0)   // 成功但无延迟数据：不进分子也不进分母

	if avg := m.Snapshot(id).AvgLatMs; avg != 150 { // (100+200)/2，而非 /3=100
		t.Fatalf("平均延迟应 150ms(仅计有延迟样本)，实际 %d", avg)
	}
}
