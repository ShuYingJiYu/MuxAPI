package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

// saveTestAttempt 直接构造一条带 outcome/protocol 的尝试，用于覆盖
// success 之外的计费形态。
func saveTestAttempt(t *testing.T, st *Store, requestID string, upstreamID int64,
	model, protocol, outcome string, at int64, inputTokens, outputTokens, cachedTokens int64) {
	t.Helper()
	requestTime := time.Unix(at, 0)
	if !st.EnqueueRequest(RequestRecord{
		RequestID: requestID, FinalUpstreamID: upstreamID, Model: model,
		Status: 200, Outcome: outcome, CreatedAt: requestTime, CompletedAt: requestTime,
		Attempts: []RequestAttemptRecord{{
			AttemptNo: 1, UpstreamID: upstreamID, Status: 200, Outcome: outcome,
			Protocol: protocol, InputTokens: inputTokens, OutputTokens: outputTokens,
			CachedTokens: cachedTokens, CreatedAt: requestTime, CompletedAt: requestTime,
		}},
	}) {
		t.Fatal("request audit queue rejected test record")
	}
	if err := st.FlushRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newUsageTestStore(t *testing.T, name string) (*Store, *upstream.Upstream) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u := &upstream.Upstream{
		Name: "provider", BaseURL: "https://example.com", APIKey: "sk-test",
		Protocol: "openai", BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	return st, u
}

// partial 的 token 已经被上游生成并计费，必须计入本地理论费用；
// 只统计 success 会系统性压低理论值，把偏差单向推成「上游多收」的误报。
func TestBillingWindowUsageIncludesPartialAttempts(t *testing.T) {
	st, u := newUsageTestStore(t, "usage-partial.db")
	saveTestAttempt(t, st, "req-success", u.ID, "gpt-test", "openai", "success",
		1_700_000_100, 1000, 500, 0)
	saveTestAttempt(t, st, "req-partial", u.ID, "gpt-test", "openai", "partial",
		1_700_000_200, 800, 400, 0)

	usage, err := st.ListBillingWindowUsage(u.ID, 1_700_000_000, 1_700_000_300)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("expected one model group, got %d", len(usage))
	}
	if usage[0].RequestCount != 2 {
		t.Fatalf("partial attempt must be counted, got %d", usage[0].RequestCount)
	}
	if usage[0].InputTokens != 1800 || usage[0].OutputTokens != 900 {
		t.Fatalf("partial tokens must be summed: %+v", usage[0])
	}
	if usage[0].MissingUsageCount != 0 {
		t.Fatalf("a partial attempt with output tokens is complete: %+v", usage[0])
	}
}

// canceled 默认不计入：上游断连后是否照收各家不同，宁可低估也不凭猜测加账。
func TestBillingWindowUsageExcludesCanceledByDefault(t *testing.T) {
	if billingCountsCanceled {
		t.Skip("billingCountsCanceled is enabled for this build")
	}
	st, u := newUsageTestStore(t, "usage-canceled.db")
	saveTestAttempt(t, st, "req-canceled", u.ID, "gpt-test", "openai", "canceled",
		1_700_000_100, 900, 300, 0)
	usage, err := st.ListBillingWindowUsage(u.ID, 1_700_000_000, 1_700_000_300)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 0 {
		t.Fatalf("canceled attempts must stay out by default: %+v", usage)
	}
}

// 流在 usage 事件前断掉：partial 且无 output，必须记为用量不完整，
// 否则会按 output=0 定价，静默压低理论费用。
func TestBillingWindowUsageFlagsPartialWithoutOutput(t *testing.T) {
	st, u := newUsageTestStore(t, "usage-incomplete.db")
	saveTestAttempt(t, st, "req-truncated", u.ID, "gpt-test", "openai", "partial",
		1_700_000_100, 1200, 0, 0)
	usage, err := st.ListBillingWindowUsage(u.ID, 1_700_000_000, 1_700_000_300)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].MissingUsageCount != 1 {
		t.Fatalf("truncated stream must be flagged incomplete: %+v", usage)
	}
}

// success 且只有 prompt token 是合法形态（部分上游不回报 output），不应误判。
func TestBillingWindowUsageAcceptsSuccessWithoutOutput(t *testing.T) {
	st, u := newUsageTestStore(t, "usage-prompt-only.db")
	saveTestAttempt(t, st, "req-prompt-only", u.ID, "gpt-test", "openai", "success",
		1_700_000_100, 1200, 0, 0)
	usage, err := st.ListBillingWindowUsage(u.ID, 1_700_000_000, 1_700_000_300)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].MissingUsageCount != 0 {
		t.Fatalf("prompt-only success must stay complete: %+v", usage)
	}
}

// 协议按尝试行快照：事后改渠道协议不得改变历史用量的 cached_tokens 口径。
func TestBillingWindowUsageUsesSnapshotProtocol(t *testing.T) {
	st, u := newUsageTestStore(t, "usage-protocol.db")
	// 当次尝试走 claude 协议（input 不含 cache_read）。
	saveTestAttempt(t, st, "req-claude", u.ID, "claude-test", "claude", "success",
		1_700_000_100, 1000, 200, 300)
	// 事后把渠道协议改成 openai。
	u.Protocol = "openai"
	if err := st.Update(u); err != nil {
		t.Fatal(err)
	}
	usage, err := st.ListBillingWindowUsage(u.ID, 1_700_000_000, 1_700_000_300)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].Protocol != "claude" {
		t.Fatalf("historical rows must keep their snapshot protocol: %+v", usage)
	}
	// claude 口径不扣 cached：1000×0.001 + 200×0.002 + 300×0.0005 = 1.55
	price := ModelPricing{
		InputCostPerToken: billingFloat(0.001), OutputCostPerToken: billingFloat(0.002),
		CacheReadInputTokenCost: billingFloat(0.0005),
	}
	cost, complete := usageListCost(usage[0], price)
	if !complete || math.Abs(cost-1.55) > 1e-9 {
		t.Fatalf("claude basis cost = %v (complete=%v), want 1.55", cost, complete)
	}
}
