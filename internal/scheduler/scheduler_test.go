package scheduler

import (
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/upstream"
)

// 验证核心痛点：A>B>C 优先级，杀 A 切 B，A 恢复后自动回切到 A。
func TestStrictPriorityFailback(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Priority: 2, Weight: 1, Enabled: true},
		{ID: 3, Name: "C", Priority: 3, Weight: 1, Enabled: true},
	}
	// 失败 1 次即熔断，冷却 50ms，便于测试
	hm := health.New(1, 50*time.Millisecond)
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)

	// 1. 初始：必选 A（最高优先级）
	if u, _ := s.Pick(0, ""); u.Name != "A" {
		t.Fatalf("初始应选 A，实际 %s", u.Name)
	}

	// 2. A 失败 → 熔断 → 应切到 B
	hm.Report(1, "", false, 0)
	if u, _ := s.Pick(0, ""); u.Name != "B" {
		t.Fatalf("A 熔断后应选 B，实际 %s", u.Name)
	}

	// 3. B 也用着，但 A 冷却未到，仍是 B（验证不会乱跳）
	if u, _ := s.Pick(0, ""); u.Name != "B" {
		t.Fatalf("A 冷却中应继续 B，实际 %s", u.Name)
	}

	// 4. A 冷却到期 + 探测成功恢复 → 应自动回切到 A（核心 failback）
	time.Sleep(60 * time.Millisecond)
	hm.IsAvailable(1, "")   // 触发 Open→HalfOpen
	hm.Report(1, "", true, 100) // 探测成功 → Closed
	if u, _ := s.Pick(0, ""); u.Name != "A" {
		t.Fatalf("A 恢复后应回切到 A，实际 %s ← 这是核心痛点", u.Name)
	}
}

// 验证全挂时返回错误
func TestAllDown(t *testing.T) {
	ups := []*upstream.Upstream{{ID: 1, Name: "A", Priority: 1, Enabled: true}}
	hm := health.New(1, time.Hour)
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)
	hm.Report(1, "", false, 0)
	if _, err := s.Pick(0, ""); err != ErrNoUpstream {
		t.Fatalf("全挂应返回 ErrNoUpstream，实际 %v", err)
	}
}

// 模型级隔离：A 的 modelX 熔断时该模型请求切 B，但 modelY 仍走 A。
func TestModelLevelRouting(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Priority: 2, Weight: 1, Enabled: true},
	}
	hm := health.New(1, time.Hour) // 一次失败即熔断
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)

	// A 的 modelX 失败 → 熔断 (A, modelX)
	hm.Report(1, "modelX", false, 0)

	// modelX 请求：A 不可用 → 切 B
	if u, _ := s.Pick(0, "modelX"); u.Name != "B" {
		t.Fatalf("modelX 在 A 熔断后应切 B，实际 %s", u.Name)
	}
	// modelY 请求：A 的该模型没事 → 仍走 A（核心：模型级隔离）
	if u, _ := s.Pick(0, "modelY"); u.Name != "A" {
		t.Fatalf("modelY 不受 modelX 熔断影响，应仍走 A，实际 %s", u.Name)
	}
}

// 验证同优先级层按权重分流：A:B = 3:1，A 被选概率约 75%。
func TestSamePriorityWeighted(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 3, Enabled: true},
		{ID: 2, Name: "B", Priority: 1, Weight: 1, Enabled: true},
	}
	hm := health.New(1, time.Hour)
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)
	cnt := map[string]int{}
	for i := 0; i < 4000; i++ {
		u, _ := s.Pick(0, "")
		cnt[u.Name]++
	}
	ratio := float64(cnt["A"]) / 4000.0
	if ratio < 0.68 || ratio > 0.82 {
		t.Fatalf("A 权重3应约75%%，实际 %.2f (A=%d B=%d)", ratio, cnt["A"], cnt["B"])
	}
}

// P2C：同层等权两候选，给 B 灌高延迟、A 灌低延迟，
// 选路应统计上明显偏向低延迟的 A（P2C 是 2 选 1，偏向≠绝对，给宽阈值）。
func TestP2CLatencyAware(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Priority: 1, Weight: 1, Enabled: true},
	}
	hm := health.New(100, time.Hour) // 高阈值避免熔断
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)

	// A 低延迟 50ms、B 高延迟 500ms，各喂多次让 EWMA 稳定
	for i := 0; i < 20; i++ {
		hm.Report(1, "", true, 50)
		hm.Report(2, "", true, 500)
	}

	cnt := map[string]int{}
	for i := 0; i < 4000; i++ {
		u, _ := s.Pick(0, "")
		cnt[u.Name]++
	}
	// 两候选等权时各以 1/2 概率成为抽样对，配对后必选 A：
	// 仅当两次都抽到 B(约 1/4)才会得到 B。理论 A≈75%，给保守阈值。
	ratioA := float64(cnt["A"]) / 4000.0
	if ratioA < 0.6 {
		t.Fatalf("P2C 应明显偏向低延迟的 A，实际 A=%.2f (A=%d B=%d)", ratioA, cnt["A"], cnt["B"])
	}
}

// P2C 冷启动：新上游 EWMA 未知(0) 视为最优，确保能拿到流量探数据。
func TestP2CColdStart(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "Old", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "New", Priority: 1, Weight: 1, Enabled: true},
	}
	hm := health.New(100, time.Hour)
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)

	// Old 已有延迟数据，New 完全没有(EWMA=0)
	for i := 0; i < 10; i++ {
		hm.Report(1, "", true, 100)
	}

	cnt := map[string]int{}
	for i := 0; i < 4000; i++ {
		u, _ := s.Pick(0, "")
		cnt[u.Name]++
	}
	// New 冷启动视为最优 → 配对后必胜，应明显多于 Old（理论 New≈75%）
	if cnt["New"] < cnt["Old"] {
		t.Fatalf("冷启动新上游应优先探数据，New 应多于 Old，实际 New=%d Old=%d", cnt["New"], cnt["Old"])
	}
}

