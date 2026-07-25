package store

import (
	"math"
	"testing"
	"time"
)

// 关键回归：窗口内倍率调整过时，理论值必须逐对累加。
// 生产上 wangwang 就是被「末端单一倍率 × 整窗累计原价」算出 14.3% 假偏差，
// 误报成上游超收 —— 实际是倍率从 0.08 调到 0.07 的时点落差。
func TestBillingAuditRangeUsesPerPairMultiplier(t *testing.T) {
	st, u := newRangeTestStore(t)
	installTestPricing(t, st)
	now := time.Now()
	// 三次采集＝两个对：第一对倍率 0.08，第二对 0.07，各消耗 100 原价。
	// 上游按各自倍率正确扣费：100×0.08 + 100×0.07 = 15
	steps := []struct {
		multiplier float64
		listCost   float64
		actualCost float64
	}{
		{0.08, 0, 0},
		{0.08, 100, 8},
		{0.07, 200, 15},
	}
	for i, step := range steps {
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, Remaining: billingFloat(1000 - step.actualCost),
			EffectiveMultiplier: billingFloat(step.multiplier),
			ReportedListCost:    billingFloat(step.listCost),
			ReportedActualCost:  billingFloat(step.actualCost),
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
		t.Fatal("multiplier change must be detected")
	}
	// 逐对：100×0.08 + 100×0.07 = 15；单一末端倍率会算成 200×0.07 = 14
	if audit.TheoreticalCost == nil || math.Abs(*audit.TheoreticalCost-15) > 1e-9 {
		t.Fatalf("theoretical cost must accumulate per pair, got %v want 15", audit.TheoreticalCost)
	}
	if audit.Deviation == nil || math.Abs(*audit.Deviation) > 1e-9 {
		t.Fatalf("a correctly billing provider must show ~0 deviation, got %v", audit.Deviation)
	}
	if audit.Status != "ok" {
		t.Fatalf("must not warn on a mid-window multiplier change: status=%s reason=%s",
			audit.Status, audit.Reason)
	}
}

// 倍率变动过、但理论值无法逐对算出时，偏差不可用于判定超收，只保留说明。
func TestEvaluateBillingAuditWithholdsWarningOnUntrustedMultiplierChange(t *testing.T) {
	// theoretical=200×0.07=14，actual=20 → 偏差 6 远超容差 1.4，必然进入告警分支
	reported, actual, multiplier := 200.0, 20.0, 0.07
	audit := BillingAudit{
		ReportedListCost:  &reported,
		ActualCost:        &actual,
		MultiplierChanged: true,
		// theoreticalPerPair 为 false —— 只能用单一倍率估算
	}
	evaluateBillingAudit(&audit, &multiplier)

	if audit.Status == "warning" {
		t.Fatalf("an untrusted aggregate must not warn: reason=%s", audit.Reason)
	}
	if audit.Reason != "multiplier_changed" {
		t.Fatalf("reason should point at the multiplier change, got %q", audit.Reason)
	}
}

// 倍率未变动时，超出容差仍必须告警 —— 守卫不能把真实超收也吞掉。
func TestEvaluateBillingAuditStillWarnsWithStableMultiplier(t *testing.T) {
	reported, actual, multiplier := 100.0, 20.0, 0.1
	audit := BillingAudit{ReportedListCost: &reported, ActualCost: &actual}
	evaluateBillingAudit(&audit, &multiplier)

	if audit.Status != "warning" || audit.Reason != "actual_cost_exceeded" {
		t.Fatalf("stable-multiplier overcharge must warn: status=%s reason=%s",
			audit.Status, audit.Reason)
	}
}
