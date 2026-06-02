package health

import (
	"testing"
	"time"
)

func TestBreakerStateMachine(t *testing.T) {
	m := New(3, 50*time.Millisecond)
	const id = int64(1)

	// 初始 CLOSED 可用
	if !m.IsAvailable(id) || m.Snapshot(id).State != "CLOSED" {
		t.Fatal("初始应 CLOSED 可用")
	}
	// 失败未达阈值，仍 CLOSED
	m.Report(id, false, 0)
	m.Report(id, false, 0)
	if !m.IsAvailable(id) {
		t.Fatal("2次失败<阈值3，应仍可用")
	}
	// 第3次失败 → OPEN，不可用
	m.Report(id, false, 0)
	if m.IsAvailable(id) || m.Snapshot(id).State != "OPEN" {
		t.Fatal("达阈值应 OPEN 不可用")
	}
	// 冷却到期 → IsAvailable 触发 HALF_OPEN（可用，放试探）
	time.Sleep(60 * time.Millisecond)
	if !m.IsAvailable(id) || m.Snapshot(id).State != "HALF_OPEN" {
		t.Fatalf("冷却后应 HALF_OPEN，实际 %s", m.Snapshot(id).State)
	}
	// 半开期失败 → 立即重新 OPEN
	m.Report(id, false, 0)
	if m.IsAvailable(id) {
		t.Fatal("半开失败应立即 OPEN")
	}
	// 冷却后半开 + 成功 → CLOSED 恢复
	time.Sleep(60 * time.Millisecond)
	m.IsAvailable(id) // → HALF_OPEN
	m.Report(id, true, 120)
	s := m.Snapshot(id)
	if s.State != "CLOSED" || s.Fails != 0 || s.LatencyMs != 120 {
		t.Fatalf("成功应 CLOSED 重置，实际 %+v", s)
	}
}

// 统计：Report 计入业务流量，reportProbe(探测) 不计入。
func TestTrafficStats(t *testing.T) {
	m := New(100, time.Second) // 高阈值避免熔断干扰
	const id = int64(7)

	m.Report(id, true, 100)  // 成功 100ms
	m.Report(id, true, 200)  // 成功 200ms
	m.Report(id, false, 0)   // 失败
	m.reportProbe(id, true, 999) // 探测：不应计入 reqs/延迟
	m.reportProbe(id, false, 0)  // 探测失败：不应计入

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
	m.Report(id, true, 100)
	m.Report(id, true, 100)
	m.Report(id, false, 0)
	m.Sample()
	// 窗口2：1 成功 1 失败 → 成功率 0.5（只看本窗口增量，不含上一窗口）
	m.Report(id, true, 100)
	m.Report(id, false, 0)
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
	down.Report(9, false, 0)
	down.Sample()
	if s := down.Snapshot(9).Trend[0].Status; s != statDown {
		t.Fatalf("熔断上游状态应为 down(%d)，实际 %d", statDown, s)
	}

	// 全程无流量、从未探测的上游：状态应为无数据（灰），不能误判成健康
	idle := New(3, time.Minute)
	idle.IsAvailable(5) // 仅注册，不产生流量也不探测
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
