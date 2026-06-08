package scheduler

import (
	"math"
	"sync"
	"sync/atomic"
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

// 回归：半开放行后，多个同层上游同时半开时，连续 Pick 都应选出上游（不再有单闸门占满问题）。
func TestHalfOpenGateNotExhaustedAcrossUpstreams(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Priority: 1, Weight: 1, Enabled: true},
		{ID: 3, Name: "C", Priority: 1, Weight: 1, Enabled: true},
	}
	hm := health.New(1, 20*time.Millisecond) // 一次失败即熔断，冷却 20ms
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)
	// 三个全部熔断 → 冷却到期后都进半开
	for _, id := range []int64{1, 2, 3} {
		hm.Report(id, "", false, 0)
	}
	time.Sleep(30 * time.Millisecond)
	// 连续 Pick：半开放行后每次都能选出上游（旧单闸门设计下第2次起会 ErrNoUpstream）
	for i := 0; i < 5; i++ {
		if _, err := s.Pick(0, ""); err != nil {
			t.Fatalf("第 %d 次 Pick 不应失败（半开放行），实际 %v", i+1, err)
		}
	}
}

// 回归(核心 503 修复)：严格优先级主备拓扑，主力刚进半开恢复期时，
// codex 式并发重试不应被选路阶段 503——半开放行使主力始终可选，绝不 no upstream available。
func TestHalfOpenPrimaryConcurrentNo503(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "primary", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "backup", Priority: 10, Weight: 1, Enabled: true},
	}
	hm := health.New(1, time.Hour) // 冷却极长：排除"靠冷却到期自愈"，纯验半开放行
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)
	// 主力 + 备份都熔断（模拟生产 503 时刻三上游全不可用的极端态），再让主力恢复到半开
	hm.Report(1, "", false, 0)
	hm.Report(2, "", false, 0)
	hm.ObserveProbe(1, "", true, 50) // 探测把主力 Open→Closed（模拟"探测恢复"）
	// 8 并发 Pick：应全部成功选出上游、无一 ErrNoUpstream
	var ok, fail int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Pick(0, ""); err == nil {
				atomic.AddInt64(&ok, 1)
			} else {
				atomic.AddInt64(&fail, 1)
			}
		}()
	}
	wg.Wait()
	if fail != 0 {
		t.Fatalf("主力恢复后 8 并发不应有 no upstream available，实际成功=%d 失败=%d", ok, fail)
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

// 智能路由：开启延迟加权后，对延迟差距大的同层渠道，应比 P2C 更激进地偏向快的。
// A 稳定 50ms、B 稳定 5000ms（均 100% 成功），权重比≈100:1，A 应拿绝大多数流量。
func TestWeightedRoutingFavorsFast(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Priority: 1, Weight: 1, Enabled: true},
	}
	hm := health.New(10000, time.Hour) // 高阈值避免熔断，纯看加权
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)
	s.SetRouting(func() float64 { return 30000 }, func() bool { return true })

	for i := 0; i < 20; i++ {
		hm.Report(1, "", true, 50)
		hm.Report(2, "", true, 5000)
	}
	cnt := map[string]int{}
	for i := 0; i < 4000; i++ {
		u, _ := s.Pick(0, "")
		cnt[u.Name]++
	}
	ratioA := float64(cnt["A"]) / 4000.0
	if ratioA < 0.9 { // 1/50 vs 1/5000 → A 理论≈99%
		t.Fatalf("延迟加权应压倒性偏向快的 A，实际 A=%.2f (A=%d B=%d)", ratioA, cnt["A"], cnt["B"])
	}
}

// 智能路由：成功率折进有效延迟——「快但常失败」应被压制。
// A 100ms@100%；B 100ms 但大量失败→成功率极低→有效延迟被失败超时成本拉爆→几乎不被选。
func TestWeightedRoutingPenalizesFailures(t *testing.T) {
	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Priority: 1, Weight: 1, Enabled: true},
	}
	hm := health.New(100000, time.Hour) // 阈值极高，连续失败也不熔断，纯看加权
	s := New(func(int64) []*upstream.Upstream { return ups }, hm)
	s.SetRouting(func() float64 { return 30000 }, func() bool { return true })

	for i := 0; i < 20; i++ {
		hm.Report(1, "", true, 100)
	}
	hm.Report(2, "", true, 100) // B 给一次成功定个低延迟基准
	for i := 0; i < 40; i++ {
		hm.Report(2, "", false, 0) // 然后狂失败，succ EWMA 趋近 0
	}
	cnt := map[string]int{}
	for i := 0; i < 4000; i++ {
		u, _ := s.Pick(0, "")
		cnt[u.Name]++
	}
	ratioA := float64(cnt["A"]) / 4000.0
	if ratioA < 0.95 { // B 有效延迟≈100+(0.99/0.01)*30000 巨大 → 几乎不被选
		t.Fatalf("快但常失败的 B 应被压制，A 实际=%.2f (A=%d B=%d)", ratioA, cnt["A"], cnt["B"])
	}
}

// PreviewShares：占比加和=1、慢渠道占比小、与实际选路同源。
func TestPreviewShares(t *testing.T) {
	tier := []*upstream.Upstream{
		{ID: 1, Name: "A", Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Weight: 1, Enabled: true},
		{ID: 3, Name: "C", Weight: 1, Enabled: true},
	}
	stats := func(id int64) (float64, float64) {
		switch id {
		case 1:
			return 50, 1
		case 2:
			return 200, 1
		default:
			return 5000, 1
		}
	}
	sh := PreviewShares(tier, stats, 30000)
	sum := sh[1].Share + sh[2].Share + sh[3].Share
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("占比加和应=1，实际 %.4f", sum)
	}
	if !(sh[1].Share > sh[2].Share && sh[2].Share > sh[3].Share) {
		t.Fatalf("应 A>B>C，实际 A=%.3f B=%.3f C=%.3f", sh[1].Share, sh[2].Share, sh[3].Share)
	}
	if sh[1].EffLatencyMs != 50 {
		t.Fatalf("A 有效延迟应=50，实际 %.1f", sh[1].EffLatencyMs)
	}
}

// PreviewShares 全失败渠道：有失败无成功(不是冷启动)，有效延迟应远大于正常渠道、占比趋零。
// 这是修复「死渠道被误当冷启动给最低延迟」的回归测试。
func TestPreviewSharesAllFail(t *testing.T) {
	tier := []*upstream.Upstream{
		{ID: 1, Name: "Good", Weight: 1, Enabled: true},
		{ID: 2, Name: "Dead", Weight: 1, Enabled: true},
	}
	stats := func(id int64) (float64, float64) {
		if id == 1 {
			return 1000, 1 // Good 正常 1s @100%
		}
		return 0, 0 // Dead 全失败：无成功延迟样本、成功率 0
	}
	sh := PreviewShares(tier, stats, 30000)
	if sh[2].EffLatencyMs <= sh[1].EffLatencyMs {
		t.Fatalf("死渠道有效延迟应远大于正常，实际 Dead=%.0f Good=%.0f", sh[2].EffLatencyMs, sh[1].EffLatencyMs)
	}
	if sh[2].Share > 0.05 {
		t.Fatalf("死渠道占比应趋零(<5%%)，实际 %.3f", sh[2].Share)
	}
	if sh[1].Share < 0.95 {
		t.Fatalf("正常渠道应吃下绝大多数流量(>95%%)，实际 %.3f", sh[1].Share)
	}
}

// PreviewShares 冷启动：无数据渠道代入同层最优，与已知最优公平竞争(占比相当)。
func TestPreviewSharesColdStart(t *testing.T) {
	tier := []*upstream.Upstream{
		{ID: 1, Name: "Old", Weight: 1, Enabled: true},
		{ID: 2, Name: "New", Weight: 1, Enabled: true},
	}
	stats := func(id int64) (float64, float64) {
		if id == 1 {
			return 100, 1
		}
		return 0, 1
	}
	sh := PreviewShares(tier, stats, 30000)
	if sh[2].Share < 0.4 || sh[2].Share > 0.6 {
		t.Fatalf("冷启动 New 应与 Old 公平竞争(约0.5)，实际 New=%.3f", sh[2].Share)
	}
}

// L4 回归：toleranceMs<=0 且同层全冷启动时，占比不得产生 NaN/Inf。
// 生产注入口恒正(不可达)，此为防御性兜底验证。
func TestPreviewSharesZeroToleranceNoNaN(t *testing.T) {
	tier := []*upstream.Upstream{
		{ID: 1, Name: "A", Priority: 1, Weight: 1, Enabled: true},
		{ID: 2, Name: "B", Priority: 1, Weight: 1, Enabled: true},
	}
	allCold := func(id int64) (float64, float64) { return 0, 1 } // EWMA=0 真冷启动
	for _, tol := range []float64{0, -5} {
		sh := PreviewShares(tier, allCold, tol)
		sum := 0.0
		for _, u := range tier {
			s := sh[u.ID]
			if math.IsNaN(s.Share) || math.IsInf(s.Share, 0) {
				t.Fatalf("tol=%.0f 全冷启动占比应有限，实际 id=%d Share=%v", tol, u.ID, s.Share)
			}
			if math.IsNaN(s.EffLatencyMs) || math.IsInf(s.EffLatencyMs, 0) {
				t.Fatalf("tol=%.0f 有效延迟应有限，实际 id=%d Eff=%v", tol, u.ID, s.EffLatencyMs)
			}
			sum += s.Share
		}
		if sum < 0.99 || sum > 1.01 { // 等权全冷应均分且加和=1
			t.Fatalf("tol=%.0f 占比加和应≈1，实际 %.4f", tol, sum)
		}
	}

	// EffLatency 直接传 0/负容忍线也不得除零爆 Inf
	if e := EffLatency(100, 0.5, 0); math.IsNaN(e) || math.IsInf(e, 0) {
		t.Fatalf("EffLatency tol=0 应有限，实际 %v", e)
	}
}

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

