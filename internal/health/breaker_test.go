package health

import (
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

	m.Report(id, "", true, 100)  // 成功 100ms
	m.Report(id, "", true, 200)  // 成功 200ms
	m.Report(id, "", false, 0)   // 失败
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
func TestModelFailCountsToUpstreamSnapshot(t *testing.T) {	m := New(100, time.Minute) // 高阈值避免熔断干扰
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
