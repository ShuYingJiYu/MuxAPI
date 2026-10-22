package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRouteDecisionPersistenceIsIdempotent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	selected := 0.012
	record := RouteDecisionRecord{
		RequestID: "route-1", GroupID: 2, Model: "claude-sonnet", Protocol: "claude",
		Endpoint: "/v1/messages", SessionKey: "session-a", PrefixHash: "prefix-a", CacheKey: "key-a",
		Reason: "cache forecast saves 0.1", SelectedUpstreamID: 11, ForecastWindow: 15 * time.Minute,
		ForecastRequests: 4, EstimatedInputTokens: 1000, ReusablePrefixTokens: 800,
		EstimatedOutputTokens: 200, SelectedCost: &selected, Confidence: 0.8, CacheSelected: true,
		CreatedAt: now,
		Candidates: []RouteCandidateRecord{{
			UpstreamID: 11, APIKeyHash: "hash-a", UpstreamName: "cheap", Protocol: "claude", Selected: true,
			Eligible: true, CacheSupported: true, CacheExisting: true, CacheSelected: true,
			CacheHitRate: 0.75, ForecastTotalCost: &selected, Details: json.RawMessage(`{"source":"test"}`),
		}},
	}
	id, err := st.SaveRouteDecision(record)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("route decision id should be assigned")
	}
	updatedReason := record
	updatedReason.Reason = "updated"
	updatedReason.Candidates[0].Details = json.RawMessage(`{"source":"updated"}`)
	updated, err := st.SaveRouteDecision(updatedReason)
	if err != nil {
		t.Fatal(err)
	}
	if updated != id {
		t.Fatalf("idempotent update changed id: %d -> %d", id, updated)
	}
	if got := countRows(t, st, "route_decisions"); got != 1 {
		t.Fatalf("expected one decision, got %d", got)
	}
	if got := countRows(t, st, "route_decision_candidates"); got != 1 {
		t.Fatalf("expected one candidate after replacement, got %d", got)
	}
	entry, err := st.GetRouteDecisionByRequestID("route-1")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Reason != "updated" || len(entry.Candidates) != 1 || string(entry.Candidates[0].Details) != `{"source":"updated"}` {
		t.Fatalf("unexpected decision: %+v", entry)
	}
	actualCost := 0.02
	actualInput := int64(1000)
	if err := st.CompleteRouteDecision("route-1", RouteDecisionOutcome{
		ActualCost: &actualCost, ActualInputTokens: &actualInput, Outcome: "success", CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	entry, err = st.GetRouteDecision(id)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ActualCost == nil || *entry.ActualCost != actualCost || entry.ActualOutcome != "success" {
		t.Fatalf("actual outcome not persisted: %+v", entry)
	}
	if err := st.SaveRoutingObservation(RoutingObservationRecord{
		RequestID: "route-1", AttemptNo: 1, UpstreamID: 11, Model: "claude-sonnet",
		InputTokens: 1200, CachedTokens: 200, CacheCreationTokens: 100,
		Success: true, ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	inflation, err := st.ListUpstreamTokenInflationStats(time.Hour, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := inflation[11]; got.TokenInflation != 1.5 || got.TokenInflationSamples != 1 {
		t.Fatalf("unexpected upstream token inflation: %+v", got)
	}
	if err := st.CompleteRouteDecision("missing", RouteDecisionOutcome{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing decision should return sql.ErrNoRows, got %v", err)
	}
}

func TestRouteDecisionPromptTokensDistinguishLegacyAndCanonicalUsage(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "prompt-tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	for _, id := range []string{"legacy-prompt", "canonical-prompt"} {
		if _, err := st.SaveRouteDecision(RouteDecisionRecord{
			RequestID: id, GroupID: 1, Model: "gpt-5", Protocol: "codex", SelectedUpstreamID: 1,
			EstimatedInputTokens: 100, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	input, cached := int64(100), int64(50)
	if err := st.CompleteRouteDecision("legacy-prompt", RouteDecisionOutcome{
		ActualInputTokens: &input, ActualCachedTokens: &cached, Outcome: "success", CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	normalized := true
	if err := st.CompleteRouteDecision("canonical-prompt", RouteDecisionOutcome{
		ActualInputTokens: &input, ActualInputTokensNormalized: &normalized, ActualCachedTokens: &cached,
		Outcome: "success", CompletedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.GetRouteDecisionByRequestID("legacy-prompt")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := st.GetRouteDecisionByRequestID("canonical-prompt")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ActualPromptTokens == nil || *legacy.ActualPromptTokens != 100 ||
		canonical.ActualPromptTokens == nil || *canonical.ActualPromptTokens != 150 {
		t.Fatalf("unexpected prompt token compatibility: legacy=%v canonical=%v", legacy.ActualPromptTokens, canonical.ActualPromptTokens)
	}
}

func TestRoutingObservationStatsAndCacheIsolation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "observations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	observations := []RoutingObservationRecord{
		{RequestID: "obs-1", AttemptNo: 1, UpstreamID: 7, APIKeyHash: "key-a", Model: "m",
			SessionKey: "s", PrefixHash: "p", PrefixTokens: 100, InputTokens: 120, OutputTokens: 20,
			CachedTokens: 100, CacheEligible: true, CacheHit: true, Success: true, TTFTMs: 100,
			DurationMs: 1000, ObservedAt: now.Add(-5 * time.Minute), CacheTTL: time.Hour},
		{RequestID: "obs-2", AttemptNo: 1, UpstreamID: 7, APIKeyHash: "key-a", Model: "m",
			SessionKey: "s", PrefixHash: "p", PrefixTokens: 100, InputTokens: 120, OutputTokens: 40,
			CacheCreationTokens: 100, CacheEligible: true, CacheCreated: true, Success: true, TTFTMs: 200,
			DurationMs: 2000, ObservedAt: now.Add(-4 * time.Minute), CacheTTL: time.Hour},
		{RequestID: "obs-3", AttemptNo: 1, UpstreamID: 7, APIKeyHash: "key-b", Model: "m",
			PrefixHash: "p", PrefixTokens: 100, InputTokens: 120, OutputTokens: 30,
			CacheCreationTokens: 100, CacheEligible: true, CacheCreated: true, Success: false, TTFTMs: 400,
			DurationMs: 4000, ObservedAt: now.Add(-3 * time.Minute), CacheTTL: time.Hour},
	}
	if err := st.SaveRoutingObservations(observations); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRoutingObservation(observations[0]); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, "routing_observations"); got != 3 {
		t.Fatalf("duplicate observation should be ignored, got %d", got)
	}
	stats, err := st.GetUpstreamRoutingStats(7, "m", 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 3 || stats.Successes != 2 || stats.CacheHits != 1 || stats.CacheMisses != 2 || stats.OutputPerRequest != 30 {
		t.Fatalf("unexpected upstream stats: %+v", stats)
	}
	if stats.P95TTFTMs != 200 || stats.P95DurationMs != 2000 {
		t.Fatalf("unexpected percentiles: %+v", stats)
	}
	cache, err := st.GetPrefixCacheStats("key-a", 7, "m", "p", "claude", 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if cache.HitCount != 1 || cache.MissCount != 1 || cache.CreateCount != 1 || cache.HitRate != 0.5 || !cache.Valid {
		t.Fatalf("unexpected key-a cache stats: %+v", cache)
	}
	if _, err := st.GetPrefixCacheStats("key-b", 7, "m", "p", "claude", 15*time.Minute, now); err != nil {
		// key-b has its own entry; reading it must not return key-a's cache.
		t.Fatal(err)
	}
	averages, err := st.CacheTokenAverages("key-a", 7, "m", "p")
	if err != nil || averages.Requests != 2 || averages.InputTokens != 120 ||
		averages.CacheReadTokens != 50 || averages.CacheWriteTokens != 50 || averages.CreateRate != 0.5 {
		t.Fatalf("unexpected cache token averages: %+v, err=%v", averages, err)
	}
	entries, err := st.ListRoutingObservations(RoutingObservationFilter{APIKeyHash: "key-a", Model: "m", Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected filtered observations: %d, %v", len(entries), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := st.FlushRequests(ctx); err != nil {
		t.Fatal(err)
	}
}

// AssumedCacheTTL must mirror routing.selectAdaptiveTTL exactly, otherwise the
// store fallback would stamp an ExpiresAt derived from a wrong TTL and the
// state machine would flip CacheHot → CacheExpired prematurely.
func TestAssumedCacheTTL(t *testing.T) {
	now := int64(1_700_000_000)
	// Gemini always → 1h.
	for _, p := range []string{"gemini", "google", "generativelanguage", "generatecontent"} {
		if got := AssumedCacheTTL(p, 1, 1, now-60, now); got != time.Hour {
			t.Fatalf("%s → %v want 1h", p, got)
		}
	}
	// Long session + rebuilds → 1h.
	if got := AssumedCacheTTL("claude", 20, 3, now-15*60, now); got != time.Hour {
		t.Fatalf("long/rebuilds → %v want 1h", got)
	}
	// Sparse conversation + any rebuild → 1h.
	if got := AssumedCacheTTL("claude", 2, 1, now-10*60, now); got != time.Hour {
		t.Fatalf("sparse → %v want 1h", got)
	}
	// Short session, few rebuilds → 5min.
	if got := AssumedCacheTTL("claude", 3, 1, now-3*60, now); got != 5*time.Minute {
		t.Fatalf("short → %v want 5min", got)
	}
	// No observations → 5min default.
	if got := AssumedCacheTTL("claude", 0, 0, 0, now); got != 5*time.Minute {
		t.Fatalf("empty → %v want 5min", got)
	}
}

func TestCachePricingStatsIgnoreNewTurnAndAbsentCache(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cache-pricing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	// A rolling hit: the prefix is fully cached, the new turn is uncached,
	// and a small rolling write lands in the same response. The old formulas
	// counted the new turn as uncovered prefix and as permanent inflation.
	observations := []RoutingObservationRecord{
		{RequestID: "price-1", UpstreamID: 26, APIKeyHash: "key-a", Model: "claude-opus-5",
			SessionKey: "s", PrefixHash: "p1", PrefixTokens: 100_000, InputTokens: 40_000,
			CachedTokens: 100_000, CacheCreationTokens: 3_000,
			CacheEligible: true, CacheHit: true, CacheCreated: true, Success: true,
			ObservedAt: now.Add(-time.Minute)},
		{RequestID: "price-2", UpstreamID: 24, APIKeyHash: "key-b", Model: "claude-opus-5",
			SessionKey: "s", PrefixHash: "p2", PrefixTokens: 100_000, InputTokens: 40_000,
			Success: true, ObservedAt: now.Add(-time.Minute)},
	}
	if err := st.SaveRoutingObservations(observations); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := st.SaveRoutingObservation(RoutingObservationRecord{
			RequestID: "nocache-" + strconv.Itoa(i), UpstreamID: 24, APIKeyHash: "key-b",
			Model: "claude-opus-5", PrefixTokens: 100_000, InputTokens: 1_000, Success: true,
			ObservedAt: now.Add(-time.Duration(i+1) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	coverage, err := st.CacheCoverageRatio(26, "claude-opus-5", 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(coverage-1) > 1e-9 {
		t.Fatalf("coverage = %v, want 1 (cached prefix / prefix, clamped)", coverage)
	}
	inflation, err := st.TokenInflationFactor(26)
	if err != nil {
		t.Fatal(err)
	}
	// (100k cached + 40k new turn + 3k write) / (100k prefix + 40k new turn) = 1.0214
	if math.Abs(inflation-143_000.0/140_000.0) > 1e-9 {
		t.Fatalf("inflation = %v, want %v", inflation, 143_000.0/140_000.0)
	}

	absent, err := st.RecentModelCacheEvidence(24, "claude-opus-5", 24*time.Hour, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Successes != 21 || absent.CacheUses != 0 {
		t.Fatalf("no-cache model evidence: %+v", absent)
	}
	present, err := st.RecentModelCacheEvidence(26, "claude-opus-5", 24*time.Hour, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if present.Successes != 1 || present.CacheUses != 1 {
		t.Fatalf("cache model evidence: %+v", present)
	}
}

func TestCacheCoverageRatioSplitsByModel(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "coverage-by-model.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	var observations []RoutingObservationRecord
	for i := 0; i < 8; i++ {
		observations = append(observations,
			RoutingObservationRecord{RequestID: "full-" + strconv.Itoa(i), UpstreamID: 51, APIKeyHash: "k",
				Model: "claude-opus-5-5", PrefixTokens: 100_000, CachedTokens: 100_000,
				CacheHit: true, Success: true, ObservedAt: now.Add(-time.Duration(i+1) * time.Minute)},
			RoutingObservationRecord{RequestID: "half-" + strconv.Itoa(i), UpstreamID: 51, APIKeyHash: "k",
				Model: "claude-sonnet-5", PrefixTokens: 100_000, CachedTokens: 20_000,
				CacheHit: true, Success: true, ObservedAt: now.Add(-time.Duration(i+1) * time.Minute)},
		)
	}
	if err := st.SaveRoutingObservations(observations); err != nil {
		t.Fatal(err)
	}
	full, err := st.CacheCoverageRatio(51, "claude-opus-5-5", 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(full-1) > 1e-9 {
		t.Fatalf("full-cache model coverage = %v, want 1", full)
	}
	half, err := st.CacheCoverageRatio(51, "claude-sonnet-5", 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(half-0.2) > 1e-9 {
		t.Fatalf("half-cache model coverage = %v, want 0.2", half)
	}
	// One hit is below the model sample floor, so an unseen model uses the
	// channel blend: eight full hits and eight 0.2 hits average to 0.6.
	fallback, err := st.CacheCoverageRatio(51, "claude-new", 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(fallback-0.6) > 1e-9 {
		t.Fatalf("fallback coverage = %v, want 0.6", fallback)
	}
}

func TestCacheCoverageRatioFollowsPrefixLength(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "coverage-by-length.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Second)
	var observations []RoutingObservationRecord
	for i := 0; i < 8; i++ {
		observations = append(observations,
			// Short prompts: a fixed 20k window is the whole prefix.
			RoutingObservationRecord{RequestID: "short-" + strconv.Itoa(i), UpstreamID: 51, APIKeyHash: "k",
				Model: "claude-opus-5-5", PrefixTokens: 20_000, CachedTokens: 20_000,
				CacheHit: true, Success: true, ObservedAt: now.Add(-time.Duration(i+1) * time.Minute)},
			// Long prompts: the same channel only covers 40k of a 160k prefix.
			RoutingObservationRecord{RequestID: "long-" + strconv.Itoa(i), UpstreamID: 51, APIKeyHash: "k",
				Model: "claude-opus-5-5", PrefixTokens: 160_000, CachedTokens: 40_000,
				CacheHit: true, Success: true, ObservedAt: now.Add(-time.Duration(i+1) * time.Minute)},
		)
	}
	if err := st.SaveRoutingObservations(observations); err != nil {
		t.Fatal(err)
	}
	short, err := st.CacheCoverageRatio(51, "claude-opus-5-5", 24_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(short-1) > 1e-9 {
		t.Fatalf("short prefix coverage = %v, want 1", short)
	}
	long, err := st.CacheCoverageRatio(51, "claude-opus-5-5", 150_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(long-0.25) > 1e-9 {
		t.Fatalf("long prefix coverage = %v, want 0.25", long)
	}
	// Eight hits where the provider cached twice the router estimate. The
	// surplus is returned rather than clamped, so the caller can price it.
	var surplus []RoutingObservationRecord
	for i := 0; i < 8; i++ {
		surplus = append(surplus, RoutingObservationRecord{
			RequestID: "surplus-" + strconv.Itoa(i), UpstreamID: 52, APIKeyHash: "k",
			Model: "claude-opus-5-5", PrefixTokens: 10_000, CachedTokens: 20_000,
			CacheHit: true, Success: true, ObservedAt: now.Add(-time.Duration(i+1) * time.Minute),
		})
	}
	if err := st.SaveRoutingObservations(surplus); err != nil {
		t.Fatal(err)
	}
	over, err := st.CacheCoverageRatio(52, "claude-opus-5-5", 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(over-2) > 1e-9 {
		t.Fatalf("surplus coverage = %v, want 2", over)
	}
}
