package store

import (
	"math"
	"testing"
	"time"
)

// 关键回归：本地价目不完整绝不能阻塞计费核对。
// 生产数据里四个上游的账目精确吻合(自报原价×倍率==实际扣费)，却因为少数请求
// 缺 usage 就被判 unavailable，什么结论都不给 —— 一个输入不可用毁掉整个结论。
func TestEvaluateBillingAuditRunsBillingTrackDespiteLocalGaps(t *testing.T) {
	reported, actual, multiplier := 2.3799049999997806, 0.11899524999999755, 0.05
	audit := BillingAudit{
		ReportedListCost:   &reported,
		ActualCost:         &actual,
		ActualSource:       "reported",
		LocalPricingReason: "request_usage_incomplete", // 本地估算不可信
		// ListCost 为 nil —— 本地价目缺失
	}
	evaluateBillingAudit(&audit, &multiplier)

	if audit.Status != "ok" {
		t.Fatalf("billing track must still conclude, got status=%s reason=%s", audit.Status, audit.Reason)
	}
	if audit.BillingBasis != "reported" {
		t.Fatalf("basis should come from the provider's own list price, got %q", audit.BillingBasis)
	}
	if audit.TheoreticalCost == nil || math.Abs(*audit.TheoreticalCost-0.11899525) > 1e-8 {
		t.Fatalf("theoretical cost = %v, want ~0.11899525", audit.TheoreticalCost)
	}
	if audit.Deviation == nil || math.Abs(*audit.Deviation) > 1e-8 {
		t.Fatalf("deviation should be ~0 for a correctly billing provider, got %v", audit.Deviation)
	}
	// 降级原因必须保留，供界面标注本地估算不可用
	if audit.LocalPricingReason != "request_usage_incomplete" {
		t.Fatalf("local pricing reason must survive: %q", audit.LocalPricingReason)
	}
	// 价目核对因缺本地估算而跳过
	if audit.CatalogDeviation != nil {
		t.Fatalf("catalog track must be skipped without a local estimate: %v", audit.CatalogDeviation)
	}
}

// 本地估算为 0（窗口内无请求／模型别名查不到价）时，价目核对必须跳过。
// 否则任何上游消费都会被判成「虚标」——生产上两个上游就是这样误报的。
func TestEvaluateBillingAuditSkipsCatalogTrackOnZeroLocalCost(t *testing.T) {
	local, reported, actual, multiplier := 0.0, 1.318460, 0.171400, 0.13
	audit := BillingAudit{ListCost: &local, ReportedListCost: &reported, ActualCost: &actual}
	evaluateBillingAudit(&audit, &multiplier)

	if audit.CatalogDeviation != nil || audit.CatalogRate != nil {
		t.Fatalf("zero local cost must not drive the catalog track: dev=%v rate=%v",
			audit.CatalogDeviation, audit.CatalogRate)
	}
	if audit.Reason == "catalog_cost_exceeded" {
		t.Fatal("zero local cost must not be reported as an inflated provider catalog")
	}
}

// 基准是本地估算时（上游未提供原价），偏差可能只反映本地价表低估，
// 不足以指控上游多收，故不升级为 warning。
func TestEvaluateBillingAuditDoesNotWarnOnLocalBasis(t *testing.T) {
	local, actual, multiplier := 2.387335, 4.960244, 1.0
	audit := BillingAudit{ListCost: &local, ActualCost: &actual}
	evaluateBillingAudit(&audit, &multiplier)

	if audit.BillingBasis != "local" {
		t.Fatalf("basis should degrade to local, got %q", audit.BillingBasis)
	}
	if audit.Status == "warning" {
		t.Fatalf("a local-basis overshoot must not be a warning: reason=%s", audit.Reason)
	}
	if audit.Reason != "actual_cost_exceeded_local_basis" {
		t.Fatalf("the overshoot must still be surfaced, got reason=%q", audit.Reason)
	}
	if audit.Deviation == nil || *audit.Deviation <= 0 {
		t.Fatalf("deviation should still be computed: %v", audit.Deviation)
	}
}

// 有自报原价时，超出容差仍必须告警 —— 这是「上游按自己规则多收」的真实信号。
func TestEvaluateBillingAuditWarnsOnReportedBasisOvercharge(t *testing.T) {
	local, reported, actual, multiplier := 2.0, 2.02, 0.35, 0.15
	audit := BillingAudit{ListCost: &local, ReportedListCost: &reported, ActualCost: &actual}
	evaluateBillingAudit(&audit, &multiplier)

	if audit.Status != "warning" || audit.Reason != "actual_cost_exceeded" {
		t.Fatalf("reported-basis overcharge must warn: status=%s reason=%s", audit.Status, audit.Reason)
	}
}

// 两个基准都没有才真的无从比对，且原因要报本地价目的具体降级项。
func TestEvaluateBillingAuditPrefersSpecificReasonWhenNoBasis(t *testing.T) {
	actual, multiplier := 1.0, 0.2
	audit := BillingAudit{ActualCost: &actual, LocalPricingReason: "model_price_unavailable"}
	evaluateBillingAudit(&audit, &multiplier)

	if audit.Status != "unavailable" || audit.Reason != "model_price_unavailable" {
		t.Fatalf("specific local reason should surface: status=%s reason=%s", audit.Status, audit.Reason)
	}
}

// 端到端：区间聚合下本地价目缺失也应给出计费结论。
func TestBillingAuditRangeConcludesWithoutLocalPricing(t *testing.T) {
	st, u := newRangeTestStore(t)
	installTestPricing(t, st) // 空价目表 → 本地估算不可用
	now := time.Now()
	for i := 0; i < 3; i++ {
		observed := now.Add(-time.Duration(30-i*10) * time.Minute)
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, Remaining: billingFloat(100 - float64(i)*0.5),
			EffectiveMultiplier: billingFloat(0.05),
			ReportedListCost:    billingFloat(float64(i) * 10),
			ReportedActualCost:  billingFloat(float64(i) * 0.5),
			ObservedAt:          observed.Unix(),
		}); err != nil {
			t.Fatal(err)
		}
		saveTestAttempt(t, st, "gap-"+string(rune('a'+i)), u.ID, "unpriced-model", "openai",
			"success", observed.Add(time.Minute).Unix(), 1000, 500, 0)
	}
	audit, err := st.BillingAuditRange(u.ID, LookupBillingWindow("24h"), now)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Status == "unavailable" {
		t.Fatalf("provider-reported data is sufficient to conclude: %+v", audit)
	}
	if audit.BillingBasis != "reported" {
		t.Fatalf("basis = %q, want reported", audit.BillingBasis)
	}
	// 空价目表命中更根本的原因，优先于「个别模型缺价」
	if audit.LocalPricingReason != "pricing_catalog_unavailable" {
		t.Fatalf("local gap should be recorded, got %q", audit.LocalPricingReason)
	}
	// 2 对 × 10 = 20 自报原价；× 0.05 = 1.0 理论；实际 2 对 × 0.5 = 1.0
	if audit.Deviation == nil || math.Abs(*audit.Deviation) > 1e-9 {
		t.Fatalf("deviation should be ~0: %v", audit.Deviation)
	}
}
