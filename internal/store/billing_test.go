package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

func billingFloat(value float64) *float64 { return &value }

func installTestPricing(t *testing.T, st *Store, prices ...ModelPricing) {
	t.Helper()
	if err := st.ReplaceModelPricing(prices, PricingCatalogStatus{
		Source: "LiteLLM", Version: "test", LastCheckedAt: 1_700_000_000,
		LastSuccessAt: 1_700_000_000,
	}); err != nil {
		t.Fatal(err)
	}
}

func saveTestBillingRequest(t *testing.T, st *Store, requestID string, upstreamID int64,
	model string, at int64, inputTokens, outputTokens, cachedTokens, cacheCreationTokens int64) {
	t.Helper()
	requestTime := time.Unix(at, 0)
	if !st.EnqueueRequest(RequestRecord{
		RequestID: requestID, FinalUpstreamID: upstreamID, Model: model,
		Status: 200, Outcome: "success", CreatedAt: requestTime, CompletedAt: requestTime,
		Attempts: []RequestAttemptRecord{{
			AttemptNo: 1, UpstreamID: upstreamID, Status: 200, Outcome: "success",
			InputTokens: inputTokens, OutputTokens: outputTokens, CachedTokens: cachedTokens,
			CacheCreationTokens: cacheCreationTokens, CreatedAt: requestTime, CompletedAt: requestTime,
		}},
	}) {
		t.Fatal("request audit queue rejected test record")
	}
	if err := st.FlushRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBillingStatusAndSnapshots(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "billing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", APIKey: "sk-test",
		Protocol: "codex", BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Get(u.ID)
	if err != nil || stored.BillingType != upstream.BillingSub2API {
		t.Fatalf("billing type was not persisted: %+v, err=%v", stored, err)
	}

	state := BillingStatus{
		UpstreamID: u.ID, Currency: "USD", Remaining: billingFloat(24.93),
		BillingGroup: "pro", GroupMultiplier: billingFloat(0.155),
		EffectiveMultiplier: billingFloat(0.155), ReportedListCost: billingFloat(101.45),
		ReportedActualCost: billingFloat(15.03), ObservedAt: 1_700_000_000,
		RefreshedAt: 1_700_000_001,
	}
	if err := st.SaveBillingSuccess(state); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBillingStatus(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.Remaining == nil || *got.Remaining != 24.93 ||
		got.EffectiveMultiplier == nil || *got.EffectiveMultiplier != 0.155 ||
		got.LastSuccessAt != state.RefreshedAt {
		t.Fatalf("unexpected billing state: %+v", got)
	}

	if err := st.SaveBillingFailure(u.ID, "timeout", 1_700_000_100); err != nil {
		t.Fatal(err)
	}
	failed, err := st.GetBillingStatus(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "error" || failed.Error != "timeout" || failed.Remaining == nil ||
		*failed.Remaining != 24.93 || failed.LastSuccessAt != state.RefreshedAt {
		t.Fatalf("failure should preserve the last successful values: %+v", failed)
	}

	snapshots, err := st.ListBillingSnapshots(u.ID, 10)
	if err != nil || len(snapshots) != 1 || snapshots[0].ReportedActualCost == nil ||
		*snapshots[0].ReportedActualCost != 15.03 {
		t.Fatalf("unexpected billing snapshots: %+v, err=%v", snapshots, err)
	}
	statuses, err := st.ListBillingStatuses()
	if err != nil || statuses[u.ID].Status != "error" {
		t.Fatalf("unexpected billing status map: %+v, err=%v", statuses, err)
	}
}

func TestLatestBillingAuditsPostgresQueryUsesBoundedLookup(t *testing.T) {
	st := &Store{db: &dbAdapter{postgres: true}}
	query := st.latestBillingAuditsQuery()
	if !strings.Contains(query, "ROW_NUMBER()") || !strings.Contains(query, "snapshot_rank<=2") {
		t.Fatalf("latest audit query must keep only the newest two snapshots: %s", query)
	}
}

func TestBillingAuditComparesSnapshotDeltas(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "billing-audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", APIKey: "sk-test",
		Protocol: "codex", BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	installTestPricing(t, st, ModelPricing{
		Model: "gpt-test", InputCostPerToken: billingFloat(0.001),
		OutputCostPerToken: billingFloat(0.002),
	})
	first := BillingStatus{
		UpstreamID: u.ID, Remaining: billingFloat(10),
		EffectiveMultiplier: billingFloat(0.15), ReportedListCost: billingFloat(100),
		ReportedActualCost: billingFloat(15), ObservedAt: 1_700_000_000,
	}
	if err := st.SaveBillingSuccess(first); err != nil {
		t.Fatal(err)
	}
	state, err := st.GetBillingStatus(u.ID)
	if err != nil || state.Audit == nil || state.Audit.Status != "pending" {
		t.Fatalf("first collection should be pending: %+v, err=%v", state.Audit, err)
	}
	saveTestBillingRequest(t, st, "billing-window", u.ID, "openai/gpt-test",
		1_700_000_300, 2000, 0, 0, 0)

	// 上游自报原价 2.02 与本地估算 2.0 基本一致（价目核对通过），
	// 但实际扣费 0.35 高于 2.02×0.15=0.303 —— 上游按自己的规则多收了。
	second := BillingStatus{
		UpstreamID: u.ID, Remaining: billingFloat(9.65),
		EffectiveMultiplier: billingFloat(0.15), ReportedListCost: billingFloat(102.02),
		ReportedActualCost: billingFloat(15.35), ObservedAt: 1_700_000_600,
	}
	if err := st.SaveBillingSuccess(second); err != nil {
		t.Fatal(err)
	}
	state, err = st.GetBillingStatus(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	audit := state.Audit
	if audit == nil || audit.Status != "warning" || audit.Reason != "actual_cost_exceeded" ||
		audit.BillingBasis != "reported" ||
		audit.ListCost == nil || math.Abs(*audit.ListCost-2) > 1e-9 ||
		audit.ReportedListCost == nil || math.Abs(*audit.ReportedListCost-2.02) > 1e-9 ||
		audit.TheoreticalCost == nil || math.Abs(*audit.TheoreticalCost-0.303) > 1e-9 ||
		audit.ActualCost == nil || math.Abs(*audit.ActualCost-0.35) > 1e-9 ||
		audit.BalanceSpent == nil || math.Abs(*audit.BalanceSpent-0.35) > 1e-9 ||
		audit.PriceCoverage == nil || *audit.PriceCoverage != 1 || audit.RequestCount != 1 {
		t.Fatalf("unexpected billing audit: %+v", audit)
	}
	statuses, err := st.ListBillingStatuses()
	if err != nil || statuses[u.ID].Audit == nil || statuses[u.ID].Audit.Status != "warning" {
		t.Fatalf("billing status list omitted audit: %+v, err=%v", statuses, err)
	}
}

// 上游自报原价远高于本地独立估算时，即使它按自己的原价正确应用了倍率，
// 也必须告警——余额是按虚标的原价扣的。这条是双轨判定的核心价值。
func TestBillingAuditFlagsInflatedProviderCatalog(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "billing-catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", APIKey: "sk-test",
		Protocol: "codex", BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	installTestPricing(t, st, ModelPricing{
		Model: "gpt-test", InputCostPerToken: billingFloat(0.001),
	})
	if err := st.SaveBillingSuccess(BillingStatus{
		UpstreamID: u.ID, Remaining: billingFloat(100),
		EffectiveMultiplier: billingFloat(0.15), ReportedListCost: billingFloat(0),
		ReportedActualCost: billingFloat(0), ObservedAt: 1_700_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	saveTestBillingRequest(t, st, "catalog-window", u.ID, "gpt-test",
		1_700_000_300, 2000, 0, 0, 0)
	// 本地估算 2.0，上游自报 20（虚标 10 倍）；实际扣费 3.0 = 20×0.15，
	// 倍率应用「正确」，所以计费核对通过，必须靠价目核对抓出来。
	if err := st.SaveBillingSuccess(BillingStatus{
		UpstreamID: u.ID, Remaining: billingFloat(97),
		EffectiveMultiplier: billingFloat(0.15), ReportedListCost: billingFloat(20),
		ReportedActualCost: billingFloat(3), ObservedAt: 1_700_000_600,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := st.GetBillingStatus(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	audit := state.Audit
	if audit == nil || audit.Status != "warning" || audit.Reason != "catalog_cost_exceeded" {
		t.Fatalf("inflated provider catalog must warn: %+v", audit)
	}
	if audit.CatalogDeviation == nil || math.Abs(*audit.CatalogDeviation-18) > 1e-9 {
		t.Fatalf("catalog deviation should be 20-2=18: %+v", audit.CatalogDeviation)
	}
	if audit.Deviation == nil || math.Abs(*audit.Deviation) > 1e-9 {
		t.Fatalf("billing track should agree with the provider's own basis: %+v", audit.Deviation)
	}
}

func TestBillingAuditUsesLocalPricesForNewAPI(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "billing-audit-newapi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", APIKey: "sk-test",
		BillingType: upstream.BillingNewAPI, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	installTestPricing(t, st, ModelPricing{
		Model: "gpt-test", InputCostPerToken: billingFloat(0.001),
		OutputCostPerToken: billingFloat(0.002),
	})
	saveTestBillingRequest(t, st, "newapi-window", u.ID, "gpt-test",
		1_700_000_300, 1250, 0, 0, 0)
	for index, actual := range []float64{2, 2.25} {
		if err := st.SaveBillingSuccess(BillingStatus{
			UpstreamID: u.ID, EffectiveMultiplier: billingFloat(0.2),
			ReportedActualCost: billingFloat(actual), ObservedAt: int64(1_700_000_000 + index*600),
		}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := st.GetBillingStatus(u.ID)
	if err != nil || state.Audit == nil || state.Audit.Status != "ok" ||
		state.Audit.TheoreticalCost == nil || math.Abs(*state.Audit.TheoreticalCost-0.25) > 1e-9 ||
		state.Audit.ActualCost == nil ||
		math.Abs(*state.Audit.ActualCost-0.25) > 1e-9 {
		t.Fatalf("unexpected New API billing audit: %+v, err=%v", state.Audit, err)
	}
}

func TestBillingAuditMarksMissingUsageUnavailable(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "billing-missing-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", Protocol: "codex",
		BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	installTestPricing(t, st, ModelPricing{
		Model: "gpt-test", InputCostPerToken: billingFloat(0.001),
		OutputCostPerToken: billingFloat(0.002),
	})
	if err := st.SaveBillingSuccess(BillingStatus{
		UpstreamID: u.ID, EffectiveMultiplier: billingFloat(0.2),
		ReportedActualCost: billingFloat(1), ObservedAt: 1_700_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	saveTestBillingRequest(t, st, "missing-usage", u.ID, "gpt-test",
		1_700_000_300, 0, 0, 0, 0)
	if err := st.SaveBillingSuccess(BillingStatus{
		UpstreamID: u.ID, EffectiveMultiplier: billingFloat(0.2),
		ReportedActualCost: billingFloat(2), ObservedAt: 1_700_000_600,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := st.GetBillingStatus(u.ID)
	if err != nil || state.Audit == nil || state.Audit.Status != "unavailable" ||
		state.Audit.Reason != "request_usage_incomplete" || state.Audit.MissingUsageCount != 1 {
		t.Fatalf("missing usage must not produce a warning: %+v, err=%v", state.Audit, err)
	}
}

func TestUsageListCostSeparatesCachedTokensByProtocol(t *testing.T) {
	price := ModelPricing{
		InputCostPerToken: billingFloat(0.01), OutputCostPerToken: billingFloat(0.02),
		CacheReadInputTokenCost: billingFloat(0.002), CacheWriteInputTokenCost: billingFloat(0.0125),
	}
	openAICost, ok := usageListCost(BillingWindowUsage{
		Protocol: "codex", InputTokens: 100, OutputTokens: 10, CachedTokens: 40,
	}, price)
	if !ok || math.Abs(openAICost-0.88) > 1e-9 {
		t.Fatalf("unexpected OpenAI cache cost: %f, complete=%v", openAICost, ok)
	}
	claudeCost, ok := usageListCost(BillingWindowUsage{
		Protocol: "claude", InputTokens: 60, OutputTokens: 10, CachedTokens: 40,
		CacheCreationTokens: 20,
	}, price)
	if !ok || math.Abs(claudeCost-1.13) > 1e-9 {
		t.Fatalf("unexpected Anthropic cache cost: %f, complete=%v", claudeCost, ok)
	}
}

func TestBillingTypeValidation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "billing-type.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u := &upstream.Upstream{Name: "generic", BaseURL: "https://example.com", Enabled: true}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	if u.BillingType != upstream.BillingNone {
		t.Fatalf("empty billing type should normalize to none, got %q", u.BillingType)
	}
	if u.CacheMode != upstream.CacheAuto {
		t.Fatalf("empty cache mode should normalize to auto, got %q", u.CacheMode)
	}
	u.CacheMode = upstream.CacheEnabled
	if err := st.Update(u); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Get(u.ID)
	if err != nil || loaded.CacheMode != upstream.CacheEnabled {
		t.Fatalf("cache mode round trip = %q, err=%v", loaded.CacheMode, err)
	}
	invalid := &upstream.Upstream{Name: "invalid", BaseURL: "https://example.com", BillingType: "other"}
	if err := st.Create(invalid); err == nil {
		t.Fatal("unsupported billing type should be rejected")
	}
	invalidCache := &upstream.Upstream{Name: "invalid-cache", BaseURL: "https://example.com", CacheMode: "other"}
	if err := st.Create(invalidCache); err == nil {
		t.Fatal("unsupported cache mode should be rejected")
	}
}

func TestBillingTypeChangeClearsCollectedData(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "billing-reset.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", APIKey: "sk-test",
		BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBillingSuccess(BillingStatus{
		UpstreamID: u.ID, Remaining: billingFloat(10), ObservedAt: 1_700_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	u.BillingType = upstream.BillingNone
	u.APIKey = ""
	if err := st.Update(u); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBillingStatus(u.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("billing status should be cleared, got %v", err)
	}
	snapshots, err := st.ListBillingSnapshots(u.ID, 10)
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("billing snapshots should be cleared: %+v, err=%v", snapshots, err)
	}
}

func TestGroupMaxMultiplierFiltersKnownBillingValues(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "group-multiplier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	limit := 0.2
	groupID, err := st.CreateGroupWithMaxMultiplier("cost limited", "", &limit)
	if err != nil {
		t.Fatal(err)
	}
	values := []struct {
		name       string
		multiplier *float64
	}{
		{name: "low", multiplier: billingFloat(0.15)},
		{name: "equal", multiplier: billingFloat(0.2)},
		{name: "high", multiplier: billingFloat(0.3)},
		{name: "unknown"},
	}
	for index, value := range values {
		u := &upstream.Upstream{
			Name: value.name, BaseURL: "https://" + value.name + ".example.com",
			BillingType: upstream.BillingSub2API, Enabled: true,
		}
		if err := st.Create(u); err != nil {
			t.Fatal(err)
		}
		if err := st.AddMember(groupID, u.ID, index+1, 1); err != nil {
			t.Fatal(err)
		}
		if value.multiplier != nil {
			if err := st.SaveBillingSuccess(BillingStatus{
				UpstreamID: u.ID, EffectiveMultiplier: value.multiplier,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	routable, err := st.ListEnabledByGroup(groupID)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(routable))
	for _, item := range routable {
		got[item.Name] = true
	}
	if !got["low"] || !got["equal"] || !got["unknown"] || got["high"] {
		t.Fatalf("unexpected multiplier filtering: %+v", got)
	}

	members, err := st.ListGroupMembers(groupID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.Name == "high" && !member.MultiplierBlocked {
			t.Fatal("high multiplier member should be marked as blocked")
		}
		if member.Name != "high" && member.MultiplierBlocked {
			t.Fatalf("member %s should remain eligible", member.Name)
		}
	}
	groups, err := st.ListGroups()
	if err != nil || len(groups) != 1 || groups[0].MaxMultiplier == nil || *groups[0].MaxMultiplier != limit {
		t.Fatalf("group multiplier was not persisted: %+v, err=%v", groups, err)
	}

	if err := st.UpdateGroupWithMaxMultiplier(groupID, "cost limited", "", nil); err != nil {
		t.Fatal(err)
	}
	routable, err = st.ListEnabledByGroup(groupID)
	if err != nil || len(routable) != len(values) {
		t.Fatalf("disabling the limit should restore all routes: %+v, err=%v", routable, err)
	}
	zero := 0.0
	if _, err := st.CreateGroupWithMaxMultiplier("invalid", "", &zero); err == nil {
		t.Fatal("zero multiplier limit should be rejected")
	}
}
