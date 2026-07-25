package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

func newRangeTestStore(t *testing.T) (*Store, *upstream.Upstream) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "billing-range.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", APIKey: "sk-test",
		Protocol: "codex", BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	return st, u
}

// 区间聚合必须跨多个刷新间隔累加，而不是只看最后一对快照——
// 单窗口结论会被下一轮刷新覆盖，等于每 10 分钟归零。
func TestBillingAuditRangeAccumulatesAcrossRefreshes(t *testing.T) {
	st, u := newRangeTestStore(t)
	installTestPricing(t, st, ModelPricing{
		Model: "gpt-test", InputCostPerToken: billingFloat(0.001),
	})
	now := time.Now()
	// 4 次采集 = 3 个相邻对，每对上游自报原价 +1.0、实际 +0.15。
	for i := 0; i < 4; i++ {
		observed := now.Add(-time.Duration(30-i*10) * time.Minute)
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, Remaining: billingFloat(100 - float64(i)*0.15),
			EffectiveMultiplier: billingFloat(0.15),
			ReportedListCost:    billingFloat(float64(i) * 1.0),
			ReportedActualCost:  billingFloat(float64(i) * 0.15),
			ObservedAt:          observed.Unix(),
		}); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			saveTestBillingRequest(t, st, "range-"+string(rune('a'+i)), u.ID, "gpt-test",
				observed.Add(time.Minute).Unix(), 1000, 0, 0, 0)
		}
	}

	audit, err := st.BillingAuditRange(u.ID, LookupBillingWindow("24h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if audit.SnapshotCount != 4 {
		t.Fatalf("range should span all 4 snapshots, got %d", audit.SnapshotCount)
	}
	// 3 对 × 1.0 = 3.0 自报原价；3 对 × 0.15 = 0.45 实际。
	if audit.ReportedListCost == nil || math.Abs(*audit.ReportedListCost-3.0) > 1e-9 {
		t.Fatalf("reported list cost should accumulate to 3.0: %+v", audit.ReportedListCost)
	}
	if audit.ActualCost == nil || math.Abs(*audit.ActualCost-0.45) > 1e-9 {
		t.Fatalf("actual cost should accumulate to 0.45: %+v", audit.ActualCost)
	}
	// 本地 3 条请求 × 1000 token × 0.001 = 3.0，与自报一致 → 价目核对通过。
	if audit.ListCost == nil || math.Abs(*audit.ListCost-3.0) > 1e-9 {
		t.Fatalf("local list cost should be 3.0: %+v", audit.ListCost)
	}
	if audit.Status != "ok" {
		t.Fatalf("consistent data should audit ok: %+v", audit)
	}
	if audit.WindowSeconds != 86400 {
		t.Fatalf("window seconds should be recorded: %d", audit.WindowSeconds)
	}
}

// 中途充值会让余额上升。首尾相减会把它记成负支出，逐对累加只跳过该对。
func TestBillingAuditRangeSkipsTopUpPairs(t *testing.T) {
	st, u := newRangeTestStore(t)
	installTestPricing(t, st)
	now := time.Now()
	remainings := []float64{10, 9, 50, 49} // 第 3 次采集前充值
	for i, remaining := range remainings {
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, Remaining: billingFloat(remaining),
			EffectiveMultiplier: billingFloat(0.15),
			ObservedAt:          now.Add(-time.Duration(30-i*10) * time.Minute).Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	audit, err := st.BillingAuditRange(u.ID, LookupBillingWindow("24h"), now)
	if err != nil {
		t.Fatal(err)
	}
	// 下降对：10→9 (1.0) 与 50→49 (1.0)；充值对 9→50 跳过。
	if audit.BalanceSpent == nil || math.Abs(*audit.BalanceSpent-2.0) > 1e-9 {
		t.Fatalf("top-up pair must be skipped, expected 2.0: %+v", audit.BalanceSpent)
	}
}

// 倍率在长窗口内变动是常态，只标注不放弃比对。
func TestBillingAuditRangeFlagsMultiplierChange(t *testing.T) {
	st, u := newRangeTestStore(t)
	installTestPricing(t, st)
	now := time.Now()
	for i, multiplier := range []float64{0.15, 0.15, 0.20} {
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, Remaining: billingFloat(100 - float64(i)),
			EffectiveMultiplier: billingFloat(multiplier),
			ReportedListCost:    billingFloat(float64(i)),
			ReportedActualCost:  billingFloat(float64(i) * 0.2),
			ObservedAt:          now.Add(-time.Duration(30-i*10) * time.Minute).Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	audit, err := st.BillingAuditRange(u.ID, LookupBillingWindow("24h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if !audit.MultiplierChanged {
		t.Fatalf("multiplier change must be reported: %+v", audit)
	}
	if audit.Status == "unavailable" && audit.Reason == "multiplier_changed" {
		t.Fatal("multiplier change must no longer abandon the audit")
	}
}

// 窗口外的快照不参与，且样本不足时保持 pending 而非报错。
func TestBillingAuditRangeIgnoresSnapshotsOutsideWindow(t *testing.T) {
	st, u := newRangeTestStore(t)
	installTestPricing(t, st)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, Remaining: billingFloat(100 - float64(i)),
			EffectiveMultiplier: billingFloat(0.15),
			ObservedAt:          now.Add(-time.Duration(48-i) * time.Hour).Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	audit, err := st.BillingAuditRange(u.ID, LookupBillingWindow("1h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Status != "pending" || audit.SnapshotCount != 0 {
		t.Fatalf("snapshots outside the window must be ignored: %+v", audit)
	}
}

func TestLookupBillingWindowFallsBackToDefault(t *testing.T) {
	if got := LookupBillingWindow("nonsense"); got.Key != DefaultBillingWindow {
		t.Fatalf("unknown window should fall back to the default, got %q", got.Key)
	}
	if got := LookupBillingWindow("7d"); got.Duration != 7*24*time.Hour {
		t.Fatalf("7d window duration = %v", got.Duration)
	}
}

// 清理必须保底留每上游最近 2 条，否则久无流量的上游连即时窗口都算不出来。
func TestPruneBillingSnapshotsKeepsLatestPair(t *testing.T) {
	st, u := newRangeTestStore(t)
	old := time.Now().AddDate(0, 0, -60)
	for i := 0; i < 5; i++ {
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, Remaining: billingFloat(float64(100 - i)),
			ObservedAt: old.Add(time.Duration(i) * time.Minute).Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := st.PruneBillingSnapshots(30)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("expected 3 stale snapshots removed, got %d", deleted)
	}
	remaining, err := st.ListBillingSnapshots(u.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("prune must keep the latest pair, got %d", len(remaining))
	}
}
