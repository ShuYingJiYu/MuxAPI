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
	if u, _ := s.Pick(0); u.Name != "A" {
		t.Fatalf("初始应选 A，实际 %s", u.Name)
	}

	// 2. A 失败 → 熔断 → 应切到 B
	hm.Report(1, false, 0)
	if u, _ := s.Pick(0); u.Name != "B" {
		t.Fatalf("A 熔断后应选 B，实际 %s", u.Name)
	}

	// 3. B 也用着，但 A 冷却未到，仍是 B（验证不会乱跳）
	if u, _ := s.Pick(0); u.Name != "B" {
		t.Fatalf("A 冷却中应继续 B，实际 %s", u.Name)
	}

	// 4. A 冷却到期 + 探测成功恢复 → 应自动回切到 A（核心 failback）
	time.Sleep(60 * time.Millisecond)
	hm.IsAvailable(1)   // 触发 Open→HalfOpen
	hm.Report(1, true, 100) // 探测成功 → Closed
	if u, _ := s.Pick(0); u.Name != "A" {
		t.Fatalf("A 恢复后应回切到 A，实际 %s ← 这是核心痛点", u.Name)
	}
}

// 验证全挂时返回错误
func TestAllDown(t *testing.T) {
	ups := []*upstream.Upstream{{ID: 1, Name: "A", Priority: 1, Enabled: true}}
	hm := health.New(1, time.Hour)
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)
	hm.Report(1, false, 0)
	if _, err := s.Pick(0); err != ErrNoUpstream {
		t.Fatalf("全挂应返回 ErrNoUpstream，实际 %v", err)
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
		u, _ := s.Pick(0)
		cnt[u.Name]++
	}
	ratio := float64(cnt["A"]) / 4000.0
	if ratio < 0.68 || ratio > 0.82 {
		t.Fatalf("A 权重3应约75%%，实际 %.2f (A=%d B=%d)", ratio, cnt["A"], cnt["B"])
	}
}

