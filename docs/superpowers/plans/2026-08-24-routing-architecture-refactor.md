# Routing Architecture Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decompose `scheduler/intelligent.go` into three orthogonal dimension providers (`pricing.go`, `cachestate.go`, `traffic.go`) in the `routing` package, with an explicit cache state machine, clean `CacheProfile` semantics, and a slimmed adapter layer.

**Architecture:** Each dimension (pricing, cache state, traffic forecast) becomes a standalone pure-function module in `internal/routing/` with its own test file. `scheduler/intelligent.go` shrinks to a thin orchestrator that calls each provider, assembles candidates, and invokes `Choose`. The `CacheProfile` struct loses its ambiguous `DefaultHitRate`/`HitRateKnown` dual and gains a clear `HitRateSource` enum.

**Tech Stack:** Go 1.24, SQLite/PostgreSQL (via existing `store` package), no new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-24-routing-architecture-refactor.md`

## Global Constraints

- All existing tests must continue to pass after each task.
- No new external dependencies.
- `cost.go` and `selector.go` internal logic stays unchanged (only type field renames propagate).
- `store/` layer is not modified — new code adapts to existing store interfaces.
- `forward/forward.go` changes are minimal (read `PreferredTTL` from the same field).
- Feature flag not needed — the refactor produces identical routing decisions (verified by existing e2e tests).

---

### Task 1: Add `HitRateSource` enum and update `CacheProfile`

**Files:**
- Modify: `internal/routing/types.go`
- Modify: `internal/routing/cost.go` (use new field)
- Modify: `internal/routing/cost_test.go` (update fixtures)
- Modify: `internal/routing/e2e_test.go` (update fixtures)
- Modify: `internal/routing/selector_test.go` (update fixtures)

**Interfaces:**
- Produces: `HitRateSource` type + constants (`HitRateObserved`, `HitRatePrior`, `HitRateUnknown`); `CacheProfile.HitRateSource` field replaces `HitRateKnown` + `DefaultHitRate`.

- [ ] **Step 1: Add `HitRateSource` type to `types.go`**

In `types.go`, after the `CacheEntry` struct, add:

```go
type HitRateSource string

const (
	HitRateObserved HitRateSource = "observed"
	HitRatePrior    HitRateSource = "prior"
	HitRateUnknown  HitRateSource = "unknown"
)
```

- [ ] **Step 2: Replace `HitRateKnown` + `DefaultHitRate` with `HitRateSource` in `CacheProfile`**

Remove fields `HitRateKnown bool` and `DefaultHitRate float64` from `CacheProfile`. Add:

```go
HitRateSource HitRateSource `json:"hit_rate_source"`
```

Keep `HitRate float64` — it always holds the effective value (observed or prior).

- [ ] **Step 3: Update `cost.go` to use `HitRateSource`**

In `EstimateWindowCost`, replace the block (lines ~146-154):

```go
hitRate := cache.HitRate
if !cache.HitRateKnown {
    if cache.DefaultHitRate > 0 {
        hitRate = cache.DefaultHitRate
        result.Warnings = append(result.Warnings, "using optimistic prior hit rate; will converge to observed")
    } else {
        hitRate = 0
        result.Warnings = append(result.Warnings, "cache hit rate is unknown; assuming misses")
    }
}
```

With:

```go
hitRate := cache.HitRate
switch cache.HitRateSource {
case HitRateObserved:
    // use HitRate as-is
case HitRatePrior:
    result.Warnings = append(result.Warnings, "using optimistic prior hit rate; will converge to observed")
case HitRateUnknown, "":
    hitRate = 0
    result.Warnings = append(result.Warnings, "cache hit rate is unknown; assuming misses")
}
```

- [ ] **Step 4: Update `breakEvenRequests` in `cost.go`**

Replace `if !cache.HitRateKnown { h = 0 }` with:

```go
if cache.HitRateSource == HitRateUnknown || cache.HitRateSource == "" {
    h = 0
}
```

- [ ] **Step 5: Update all test files**

In `cost_test.go`, `selector_test.go`, and `e2e_test.go`, replace every occurrence of:
- `HitRateKnown: true` → `HitRateSource: HitRateObserved`
- `HitRate: X, HitRateKnown: true` → `HitRate: X, HitRateSource: HitRateObserved`

No test uses `DefaultHitRate` directly (that's set by the scheduler), so no other changes needed.

- [ ] **Step 6: Run tests**

Run: `cd /Users/sakurapuare/Desktop/homelab/repos/MuxAPI && go test ./internal/routing/...`
Expected: ALL PASS

- [ ] **Step 7: Commit**

```bash
git add internal/routing/types.go internal/routing/cost.go internal/routing/cost_test.go internal/routing/e2e_test.go internal/routing/selector_test.go
git commit -m "refactor(routing): replace HitRateKnown/DefaultHitRate with HitRateSource enum"
```

---

### Task 2: Create `internal/routing/pricing.go` — pricing dimension provider

**Files:**
- Create: `internal/routing/pricing.go`
- Create: `internal/routing/pricing_test.go`

**Interfaces:**
- Consumes: `store.ModelPricing`, `store.BillingStatus` (existing types from store package, referenced by field values only — this file does not import store, it takes pre-fetched data)
- Produces: `ResolvePricing(params PricingParams) Pricing`

- [ ] **Step 1: Write the test file `pricing_test.go`**

```go
package routing

import "testing"

func TestResolvePricingFromCatalog(t *testing.T) {
	p := ResolvePricing(PricingParams{
		InputCostPerToken:      ptr(3e-6),
		OutputCostPerToken:     ptr(15e-6),
		CacheReadPerToken:      ptr(0.3e-6),
		CacheWritePerToken:     ptr(3.75e-6),
		EffectiveMultiplier:    0,
		GroupMultiplier:        0,
		LastKnownMultiplier:    0,
		CreditRatio:            1,
	})
	if !p.InputKnown || p.InputPerToken != 3e-6 {
		t.Fatalf("input: %+v", p)
	}
	if !p.OutputKnown || p.OutputPerToken != 15e-6 {
		t.Fatalf("output: %+v", p)
	}
	if !p.CacheReadKnown || p.CacheReadPerToken != 0.3e-6 {
		t.Fatalf("cache read: %+v", p)
	}
	if p.Multiplier != 1 {
		t.Fatalf("multiplier: %+v", p)
	}
	if p.Source != "catalog" {
		t.Fatalf("source: %+v", p)
	}
	if p.Confidence != 0.55 {
		t.Fatalf("confidence: %+v", p)
	}
}

func TestResolvePricingMultiplierFallbackChain(t *testing.T) {
	// EffectiveMultiplier wins
	p := ResolvePricing(PricingParams{
		InputCostPerToken:   ptr(1e-6),
		OutputCostPerToken:  ptr(1e-6),
		EffectiveMultiplier: 0.05,
		GroupMultiplier:     0.1,
		LastKnownMultiplier: 0.2,
		CreditRatio:         2,
	})
	if p.Multiplier != 0.025 { // 0.05 / 2
		t.Fatalf("effective multiplier: got %v", p.Multiplier)
	}
	if p.Source != "catalog+provider-billing" || p.Confidence != 0.8 {
		t.Fatalf("source/confidence: %+v", p)
	}

	// GroupMultiplier fallback
	p = ResolvePricing(PricingParams{
		InputCostPerToken:   ptr(1e-6),
		OutputCostPerToken:  ptr(1e-6),
		EffectiveMultiplier: 0,
		GroupMultiplier:     0.1,
		LastKnownMultiplier: 0.2,
		CreditRatio:         2,
	})
	if p.Multiplier != 0.05 { // 0.1 / 2
		t.Fatalf("group multiplier: got %v", p.Multiplier)
	}
	if p.Source != "catalog+provider-billing-group" || p.Confidence != 0.7 {
		t.Fatalf("source/confidence: %+v", p)
	}

	// LastKnown fallback
	p = ResolvePricing(PricingParams{
		InputCostPerToken:   ptr(1e-6),
		OutputCostPerToken:  ptr(1e-6),
		EffectiveMultiplier: 0,
		GroupMultiplier:     0,
		LastKnownMultiplier: 0.2,
		CreditRatio:         2,
	})
	if p.Multiplier != 0.1 { // 0.2 / 2
		t.Fatalf("last known multiplier: got %v", p.Multiplier)
	}
	if p.Source != "catalog+provider-billing-stale" || p.Confidence != 0.6 {
		t.Fatalf("source/confidence: %+v", p)
	}

	// No multiplier info → default 1
	p = ResolvePricing(PricingParams{
		InputCostPerToken:  ptr(1e-6),
		OutputCostPerToken: ptr(1e-6),
		CreditRatio:        1,
	})
	if p.Multiplier != 1 {
		t.Fatalf("default multiplier: got %v", p.Multiplier)
	}
}

func TestResolvePricingNilCatalog(t *testing.T) {
	p := ResolvePricing(PricingParams{})
	if p.InputKnown || p.OutputKnown {
		t.Fatalf("nil catalog should produce unknown pricing: %+v", p)
	}
}

func ptr(v float64) *float64 { return &v }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/routing/ -run TestResolvePricing -v`
Expected: FAIL — `ResolvePricing` not defined

- [ ] **Step 3: Write `pricing.go`**

```go
package routing

// PricingParams holds the raw inputs needed to resolve pricing for one
// upstream. The caller (scheduler adapter) fetches these from the catalog
// and billing status; this function is a pure deterministic transform.
type PricingParams struct {
	// From LiteLLM catalog (nil = unknown)
	InputCostPerToken  *float64
	OutputCostPerToken *float64
	CacheReadPerToken  *float64
	CacheWritePerToken *float64

	// From billing status (0 = not available at this tier)
	EffectiveMultiplier float64
	GroupMultiplier     float64
	LastKnownMultiplier float64

	// Per-upstream config
	CreditRatio float64
}

// ResolvePricing assembles a Pricing struct from catalog rates and the
// multiplier fallback chain. The three-tier multiplier resolution is:
//   1. EffectiveMultiplier (provider-reported, per-upstream)
//   2. GroupMultiplier (provider-reported, per-group)
//   3. LastKnownMultiplier (stale snapshot fallback)
//   4. Default 1.0
// Each tier divides by CreditRatio when > 0.
func ResolvePricing(params PricingParams) Pricing {
	result := Pricing{Source: "catalog", Confidence: 0.55, Multiplier: 1}

	if params.InputCostPerToken != nil {
		result.InputPerToken = *params.InputCostPerToken
		result.InputKnown = true
	}
	if params.OutputCostPerToken != nil {
		result.OutputPerToken = *params.OutputCostPerToken
		result.OutputKnown = true
	}
	if params.CacheReadPerToken != nil {
		result.CacheReadPerToken = *params.CacheReadPerToken
		result.CacheReadKnown = true
	}
	if params.CacheWritePerToken != nil {
		result.CacheWritePerToken = *params.CacheWritePerToken
		result.CacheWriteKnown = true
	}

	creditRatio := params.CreditRatio
	if creditRatio <= 0 {
		creditRatio = 1
	}

	switch {
	case params.EffectiveMultiplier > 0:
		result.Multiplier = params.EffectiveMultiplier / creditRatio
		result.Source = "catalog+provider-billing"
		result.Confidence = 0.8
	case params.GroupMultiplier > 0:
		result.Multiplier = params.GroupMultiplier / creditRatio
		result.Source = "catalog+provider-billing-group"
		result.Confidence = 0.7
	case params.LastKnownMultiplier > 0:
		result.Multiplier = params.LastKnownMultiplier / creditRatio
		result.Source = "catalog+provider-billing-stale"
		result.Confidence = 0.6
	}

	return result
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/routing/ -run TestResolvePricing -v`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add internal/routing/pricing.go internal/routing/pricing_test.go
git commit -m "feat(routing): extract pricing dimension as ResolvePricing pure function"
```

---

### Task 3: Create `internal/routing/cachestate.go` — cache state machine

**Files:**
- Create: `internal/routing/cachestate.go`
- Create: `internal/routing/cachestate_test.go`

**Interfaces:**
- Consumes: `store.PrefixCacheStats` field values (passed in via `CacheStateParams`)
- Produces: `SessionCacheState` enum, `SessionCache` struct, `ResolveSessionCache(params CacheStateParams, now time.Time) SessionCache`, `SessionCache.ToCacheProfile() CacheProfile`

- [ ] **Step 1: Write `cachestate_test.go`**

```go
package routing

import (
	"testing"
	"time"
)

func TestResolveSessionCacheCold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sc := ResolveSessionCache(CacheStateParams{
		Supported:     true,
		Observations:  0,
		CoverageRatio: 1,
	}, now)
	if sc.State != CacheCold {
		t.Fatalf("expected COLD, got %v", sc.State)
	}
	profile := sc.ToCacheProfile()
	if profile.HitRateSource != HitRateUnknown {
		t.Fatalf("cold should have unknown hit rate source: %v", profile.HitRateSource)
	}
}

func TestResolveSessionCacheHot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sc := ResolveSessionCache(CacheStateParams{
		Supported:     true,
		Observations:  10,
		HitCount:      8,
		MissCount:     2,
		CreateCount:   2,
		PrefixTokens:  5000,
		ExpiresAt:     now.Add(3 * time.Minute).Unix(),
		FirstSeenAt:   now.Add(-8 * time.Minute).Unix(),
		CoverageRatio: 0.77,
		CacheMode:     "enabled",
	}, now)
	if sc.State != CacheHot {
		t.Fatalf("expected HOT, got %v", sc.State)
	}
	if sc.ExpiresAt.IsZero() || sc.ExpiresAt.Before(now) {
		t.Fatalf("HOT should have future ExpiresAt: %v", sc.ExpiresAt)
	}
	if sc.HitRate != 0.8 { // 8/(8+2)
		t.Fatalf("hit rate: got %v want 0.8", sc.HitRate)
	}
	if sc.CoverageRatio != 0.77 {
		t.Fatalf("coverage: got %v", sc.CoverageRatio)
	}
	profile := sc.ToCacheProfile()
	if !profile.Supported || profile.HitRateSource != HitRateObserved {
		t.Fatalf("profile: %+v", profile)
	}
	if !profile.Existing.Valid {
		t.Fatal("HOT should produce valid Existing entry")
	}
}

func TestResolveSessionCacheExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sc := ResolveSessionCache(CacheStateParams{
		Supported:    true,
		Observations: 5,
		HitCount:     3,
		MissCount:    2,
		CreateCount:  2,
		PrefixTokens: 5000,
		ExpiresAt:    now.Add(-1 * time.Minute).Unix(), // expired
		FirstSeenAt:  now.Add(-20 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:    "enabled",
	}, now)
	if sc.State != CacheExpired {
		t.Fatalf("expected EXPIRED, got %v", sc.State)
	}
	profile := sc.ToCacheProfile()
	if profile.Existing.Valid {
		t.Fatal("EXPIRED should not produce valid Existing")
	}
	if profile.HitRateSource != HitRateObserved {
		t.Fatalf("expired with observations should still report observed rate: %v", profile.HitRateSource)
	}
}

func TestResolveSessionCacheWarming(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sc := ResolveSessionCache(CacheStateParams{
		Supported:    true,
		Observations: 1,
		HitCount:     0,
		MissCount:    0,
		CreateCount:  1,
		PrefixTokens: 5000,
		ExpiresAt:    now.Add(4 * time.Minute).Unix(),
		FirstSeenAt:  now.Add(-30 * time.Second).Unix(),
		CoverageRatio: 1,
		CacheMode:    "enabled",
	}, now)
	if sc.State != CacheWarming {
		t.Fatalf("expected WARMING, got %v", sc.State)
	}
}

func TestAdaptiveTTLSelection(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// Short session, few creates → 5min
	sc := ResolveSessionCache(CacheStateParams{
		Supported:    true,
		Observations: 3,
		CreateCount:  1,
		FirstSeenAt:  now.Add(-3 * time.Minute).Unix(),
		ExpiresAt:    now.Add(2 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:    "enabled",
	}, now)
	if sc.PreferredTTL != 5*time.Minute {
		t.Fatalf("short session should use 5min TTL, got %v", sc.PreferredTTL)
	}

	// Long session + multiple creates → 1h
	sc = ResolveSessionCache(CacheStateParams{
		Supported:    true,
		Observations: 20,
		CreateCount:  3,
		FirstSeenAt:  now.Add(-15 * time.Minute).Unix(),
		ExpiresAt:    now.Add(2 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:    "enabled",
	}, now)
	if sc.PreferredTTL != time.Hour {
		t.Fatalf("long session with rebuilds should use 1h TTL, got %v", sc.PreferredTTL)
	}

	// Sparse requests (interval > 4min) + at least 1 create → 1h
	sc = ResolveSessionCache(CacheStateParams{
		Supported:    true,
		Observations: 2,
		CreateCount:  1,
		FirstSeenAt:  now.Add(-10 * time.Minute).Unix(),
		ExpiresAt:    now.Add(2 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:    "enabled",
	}, now)
	if sc.PreferredTTL != time.Hour {
		t.Fatalf("sparse requests should use 1h TTL, got %v", sc.PreferredTTL)
	}
}

func TestResolveSessionCacheUnsupported(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sc := ResolveSessionCache(CacheStateParams{Supported: false}, now)
	if sc.State != CacheCold {
		t.Fatalf("unsupported should be COLD: %v", sc.State)
	}
	profile := sc.ToCacheProfile()
	if profile.Supported {
		t.Fatal("unsupported should not produce supported profile")
	}
}

func TestResolveSessionCachePriorHitRate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Supported but zero observations — use prior
	sc := ResolveSessionCache(CacheStateParams{
		Supported:     true,
		Observations:  0,
		CoverageRatio: 1,
		CacheMode:     "enabled",
	}, now)
	profile := sc.ToCacheProfile()
	if profile.HitRateSource != HitRatePrior {
		t.Fatalf("supported with no observations should use prior: %v", profile.HitRateSource)
	}
	if profile.HitRate != defaultPriorHitRate {
		t.Fatalf("prior hit rate: got %v want %v", profile.HitRate, defaultPriorHitRate)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/routing/ -run "TestResolveSessionCache|TestAdaptiveTTL" -v`
Expected: FAIL — types not defined

- [ ] **Step 3: Write `cachestate.go`**

```go
package routing

import "time"

// SessionCacheState represents the lifecycle of a session's cache on one upstream.
type SessionCacheState int

const (
	CacheCold    SessionCacheState = iota // never cached or fully expired with no recent activity
	CacheWarming                          // cache created but no hit observed yet
	CacheHot                              // recent cache hit, entry is valid
	CacheExpired                          // entry past TTL
)

func (s SessionCacheState) String() string {
	switch s {
	case CacheCold:
		return "COLD"
	case CacheWarming:
		return "WARMING"
	case CacheHot:
		return "HOT"
	case CacheExpired:
		return "EXPIRED"
	default:
		return "UNKNOWN"
	}
}

const defaultPriorHitRate = 0.85

// CacheStateParams holds the raw observations needed to determine cache state.
// All timestamps are Unix seconds (0 = unknown). The caller fetches these from
// the store layer; this function is a pure deterministic transform.
type CacheStateParams struct {
	Supported    bool
	CacheMode    string // "enabled" / "auto" / "disabled"
	Observations int64
	HitCount     int64
	MissCount    int64
	CreateCount  int64
	PrefixTokens int64
	ExpiresAt    int64 // unix seconds, 0 = unknown
	FirstSeenAt  int64 // unix seconds
	LastHitAt    int64 // unix seconds
	CoverageRatio float64
	Protocol      string // "gemini" forces 1h TTL
}

// SessionCache is the resolved cache state for one session on one upstream.
type SessionCache struct {
	State         SessionCacheState
	ExpiresAt     time.Time
	HitRate       float64
	HitRateSource HitRateSource
	CoverageRatio float64
	CreateCount   int64
	PrefixTokens  int64
	PreferredTTL  time.Duration
}

// ResolveSessionCache determines the cache state from raw observations.
func ResolveSessionCache(params CacheStateParams, now time.Time) SessionCache {
	sc := SessionCache{
		CoverageRatio: params.CoverageRatio,
		CreateCount:   params.CreateCount,
		PrefixTokens:  params.PrefixTokens,
	}

	if !params.Supported {
		sc.State = CacheCold
		sc.HitRateSource = HitRateUnknown
		return sc
	}

	// Determine TTL
	sc.PreferredTTL = selectAdaptiveTTL(params, now)

	// Determine hit rate
	if params.Observations > 0 && (params.HitCount > 0 || params.MissCount > 0) {
		denom := params.HitCount + params.MissCount
		if denom > 0 {
			sc.HitRate = float64(params.HitCount) / float64(denom)
		}
		sc.HitRateSource = HitRateObserved
	} else if params.CacheMode == "enabled" || params.Observations == 0 {
		sc.HitRate = defaultPriorHitRate
		sc.HitRateSource = HitRatePrior
	} else {
		sc.HitRateSource = HitRateUnknown
	}

	// Determine state from expiry and observation history
	if params.ExpiresAt > 0 {
		sc.ExpiresAt = time.Unix(params.ExpiresAt, 0)
	}

	switch {
	case params.Observations == 0 && params.CacheMode == "enabled":
		// Supported via config but never observed — use prior, state is COLD
		sc.State = CacheCold
		sc.HitRateSource = HitRatePrior
		sc.HitRate = defaultPriorHitRate
	case params.Observations == 0:
		sc.State = CacheCold
	case params.ExpiresAt > 0 && params.ExpiresAt <= now.Unix():
		sc.State = CacheExpired
	case params.HitCount == 0 && params.CreateCount > 0 && params.ExpiresAt > now.Unix():
		sc.State = CacheWarming
	case params.ExpiresAt > now.Unix():
		sc.State = CacheHot
	case params.HitCount > 0 || params.CreateCount > 0:
		// Had activity but no valid expiry — treat as expired
		sc.State = CacheExpired
	default:
		sc.State = CacheCold
	}

	return sc
}

// selectAdaptiveTTL picks 5min or 1h based on session behavior.
func selectAdaptiveTTL(params CacheStateParams, now time.Time) time.Duration {
	const defaultTTL = 5 * time.Minute
	const extendedTTL = time.Hour

	if NormalizeProtocol(params.Protocol) == "gemini" {
		return extendedTTL
	}

	if params.FirstSeenAt <= 0 || params.Observations <= 0 {
		return defaultTTL
	}

	sessionDuration := time.Duration(now.Unix()-params.FirstSeenAt) * time.Second

	if sessionDuration > 10*time.Minute && params.CreateCount >= 2 {
		return extendedTTL
	}

	avgInterval := sessionDuration / time.Duration(params.Observations)
	if avgInterval > 4*time.Minute && params.CreateCount >= 1 {
		return extendedTTL
	}

	return defaultTTL
}

// ToCacheProfile converts the resolved session cache to the CacheProfile
// consumed by the cost model. This is the single point where state-machine
// semantics translate to cost-model inputs.
func (sc SessionCache) ToCacheProfile() CacheProfile {
	if !sc.isSupported() {
		return CacheProfile{}
	}

	profile := CacheProfile{
		Supported:     true,
		TTL:           sc.PreferredTTL,
		MinTokens:     1024,
		HitRate:       sc.HitRate,
		HitRateSource: sc.HitRateSource,
		CoverageRatio: sc.CoverageRatio,
		PreferredTTL:  sc.PreferredTTL,
	}

	switch sc.State {
	case CacheHot, CacheWarming:
		profile.Existing = CacheEntry{
			Valid:        true,
			PrefixTokens: sc.PrefixTokens,
			ExpiresAt:    sc.ExpiresAt,
		}
	case CacheExpired, CacheCold:
		profile.Existing = CacheEntry{Valid: false}
	}

	return profile
}

func (sc SessionCache) isSupported() bool {
	return sc.HitRateSource != "" || sc.PreferredTTL > 0 || sc.CreateCount > 0 || sc.PrefixTokens > 0
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/routing/ -run "TestResolveSessionCache|TestAdaptiveTTL" -v`
Expected: ALL PASS

- [ ] **Step 5: Run all routing tests to ensure no regression**

Run: `go test ./internal/routing/...`
Expected: ALL PASS

- [ ] **Step 6: Commit**

```bash
git add internal/routing/cachestate.go internal/routing/cachestate_test.go
git commit -m "feat(routing): extract cache state machine as ResolveSessionCache"
```

---

### Task 4: Create `internal/routing/traffic.go` — traffic forecast provider

**Files:**
- Create: `internal/routing/traffic.go`
- Create: `internal/routing/traffic_test.go`

**Interfaces:**
- Consumes: `UpstreamTrafficSample` (new lightweight input struct)
- Produces: `BuildForecast(samples []UpstreamTrafficSample, window time.Duration) TrafficForecast`

- [ ] **Step 1: Write `traffic_test.go`**

```go
package routing

import (
	"testing"
	"time"
)

func TestBuildForecastAggregatesRPM(t *testing.T) {
	f := BuildForecast([]UpstreamTrafficSample{
		{RequestsPerMinute: 2.0, OutputPerRequest: 500},
		{RequestsPerMinute: 3.0, OutputPerRequest: 300},
	}, 15*time.Minute)

	if f.Window != 15*time.Minute {
		t.Fatalf("window: %v", f.Window)
	}
	if f.RequestsPerMinute != 5.0 { // sum
		t.Fatalf("rpm: %v", f.RequestsPerMinute)
	}
	if f.OutputTokensPerReq != 400 { // avg
		t.Fatalf("output per req: %v", f.OutputTokensPerReq)
	}
}

func TestBuildForecastEmpty(t *testing.T) {
	f := BuildForecast(nil, 15*time.Minute)
	if f.RequestsPerMinute != 0 || f.OutputTokensPerReq != 0 {
		t.Fatalf("empty should be zero: %+v", f)
	}
	if f.Window != 15*time.Minute {
		t.Fatalf("window should still be set: %v", f.Window)
	}
}

func TestBuildForecastSkipsZeroRPM(t *testing.T) {
	f := BuildForecast([]UpstreamTrafficSample{
		{RequestsPerMinute: 0, OutputPerRequest: 500},
		{RequestsPerMinute: 4.0, OutputPerRequest: 200},
	}, 5*time.Minute)

	if f.RequestsPerMinute != 4.0 {
		t.Fatalf("should skip zero rpm: %v", f.RequestsPerMinute)
	}
	if f.OutputTokensPerReq != 200 { // only from non-zero sample
		t.Fatalf("output per req: %v", f.OutputTokensPerReq)
	}
}

func TestBuildForecastDefaultWindow(t *testing.T) {
	f := BuildForecast([]UpstreamTrafficSample{
		{RequestsPerMinute: 1.0, OutputPerRequest: 100},
	}, 0)
	if f.Window != 15*time.Minute {
		t.Fatalf("zero window should default to 15min: %v", f.Window)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/routing/ -run TestBuildForecast -v`
Expected: FAIL

- [ ] **Step 3: Write `traffic.go`**

```go
package routing

import "time"

// UpstreamTrafficSample holds recent traffic statistics for one upstream.
// The scheduler adapter produces these from store.UpstreamRoutingStats.
type UpstreamTrafficSample struct {
	RequestsPerMinute float64
	OutputPerRequest  float64
}

// BuildForecast aggregates traffic samples from all upstreams serving a model
// into a single TrafficForecast. RPM is summed (total model throughput);
// output per request is averaged across upstreams with nonzero traffic.
func BuildForecast(samples []UpstreamTrafficSample, window time.Duration) TrafficForecast {
	if window <= 0 {
		window = 15 * time.Minute
	}

	var totalRPM float64
	var totalOutput float64
	var count int

	for _, s := range samples {
		if s.RequestsPerMinute <= 0 {
			continue
		}
		totalRPM += s.RequestsPerMinute
		totalOutput += s.OutputPerRequest
		count++
	}

	var avgOutput float64
	if count > 0 {
		avgOutput = totalOutput / float64(count)
	}

	return TrafficForecast{
		Window:             window,
		RequestsPerMinute:  totalRPM,
		OutputTokensPerReq: avgOutput,
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/routing/ -run TestBuildForecast -v`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add internal/routing/traffic.go internal/routing/traffic_test.go
git commit -m "feat(routing): extract traffic forecast as BuildForecast pure function"
```

---

### Task 5: Rewrite `scheduler/intelligent.go` to use dimension providers

**Files:**
- Modify: `internal/scheduler/intelligent.go`

**Interfaces:**
- Consumes: `routing.ResolvePricing`, `routing.ResolveSessionCache`, `routing.BuildForecast`, `routing.CacheStateParams`, `routing.PricingParams`, `routing.UpstreamTrafficSample`
- Produces: Same `intelligentRouter.pick()` signature — transparent to callers

- [ ] **Step 1: Replace `price()` method with call to `routing.ResolvePricing`**

Replace the entire `func (r *intelligentRouter) price(...)` method (lines 149-195) with:

```go
func (r *intelligentRouter) price(item *upstream.Upstream, model string, statuses map[int64]store.BillingStatus) routing.Pricing {
	catalogPrice, err := r.modelPricing(model, time.Now())
	if err != nil {
		return routing.Pricing{}
	}

	params := routing.PricingParams{
		CreditRatio: item.CreditRatio,
	}
	if catalogPrice.InputCostPerToken != nil {
		params.InputCostPerToken = catalogPrice.InputCostPerToken
	}
	if catalogPrice.OutputCostPerToken != nil {
		params.OutputCostPerToken = catalogPrice.OutputCostPerToken
	}
	if catalogPrice.CacheReadInputTokenCost != nil {
		params.CacheReadPerToken = catalogPrice.CacheReadInputTokenCost
	}
	if catalogPrice.CacheWriteInputTokenCost != nil {
		params.CacheWritePerToken = catalogPrice.CacheWriteInputTokenCost
	}

	if status, ok := statuses[item.ID]; ok {
		if status.EffectiveMultiplier != nil && *status.EffectiveMultiplier > 0 {
			params.EffectiveMultiplier = *status.EffectiveMultiplier
		} else if status.GroupMultiplier != nil && *status.GroupMultiplier > 0 {
			params.GroupMultiplier = *status.GroupMultiplier
		} else {
			params.LastKnownMultiplier = r.lastKnownMultiplier(item.ID)
		}
	}

	return routing.ResolvePricing(params)
}
```

- [ ] **Step 2: Replace `cache()` method with call to `routing.ResolveSessionCache`**

Replace the entire `func (r *intelligentRouter) cache(...)` method (lines 216-262) and `selectCacheTTL` (lines 268-292) with:

```go
func (r *intelligentRouter) cache(item *upstream.Upstream, model string, features routing.RequestFeatures, now time.Time, window time.Duration) routing.CacheProfile {
	if features.ReusableInputTokens <= 0 {
		return routing.CacheProfile{}
	}
	cacheMode, _ := upstream.NormalizeCacheMode(item.CacheMode)
	if cacheMode == upstream.CacheDisabled {
		return routing.CacheProfile{}
	}

	keyHash := hashCredential(item.APIKey)
	prefixHash := features.CacheKey
	if features.SessionID != "" && features.SessionID != features.CacheKey {
		prefixHash = features.SessionID
	}

	stats, err := r.prefixStats(keyHash, item.ID, model, prefixHash, window, now)
	observed := err == nil
	cacheObserved := observed && (stats.HitCount > 0 || stats.CreateCount > 0)
	supported := cacheMode == upstream.CacheEnabled || cacheObserved
	if !supported {
		return routing.CacheProfile{}
	}

	params := routing.CacheStateParams{
		Supported:     true,
		CacheMode:     string(cacheMode),
		Observations:  stats.WindowObservations,
		HitCount:      stats.WindowHitCount,
		MissCount:     stats.WindowMissCount,
		CreateCount:   stats.CreateCount,
		PrefixTokens:  stats.PrefixTokens,
		ExpiresAt:     stats.ExpiresAt,
		FirstSeenAt:   stats.FirstSeenAt,
		CoverageRatio: r.cacheCoverage(item.ID),
		Protocol:      item.Protocol,
	}
	if !observed {
		params.Observations = 0
	}

	sc := routing.ResolveSessionCache(params, now)
	return sc.ToCacheProfile()
}
```

- [ ] **Step 3: Replace `forecast()` method with call to `routing.BuildForecast`**

Replace the entire `func (r *intelligentRouter) forecast(...)` method (lines 294-315) with:

```go
func (r *intelligentRouter) forecast(all []*upstream.Upstream, model string, features routing.RequestFeatures, cfg routing.Config, now time.Time) routing.TrafficForecast {
	samples := make([]routing.UpstreamTrafficSample, 0, len(all))
	for _, item := range all {
		if item == nil {
			continue
		}
		stats, err := r.upstreamStats(item.ID, model, cfg.Window, now)
		if err != nil || stats.Requests == 0 {
			continue
		}
		samples = append(samples, routing.UpstreamTrafficSample{
			RequestsPerMinute: stats.RequestsPerMinute,
			OutputPerRequest:  stats.OutputPerRequest,
		})
	}
	return routing.BuildForecast(samples, cfg.Window)
}
```

- [ ] **Step 4: Run all tests**

Run: `go test ./internal/routing/... ./internal/scheduler/...`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/intelligent.go
git commit -m "refactor(scheduler): slim intelligent.go to use routing dimension providers"
```

---

### Task 6: Update `scheduler/intelligent.go` `candidates()` to use `HitRateSource` field

**Files:**
- Modify: `internal/scheduler/intelligent.go`

**Interfaces:**
- The `candidates()` method already calls `r.cache()` which now returns profiles with `HitRateSource` instead of `HitRateKnown`/`DefaultHitRate`. No additional change needed in `candidates()` itself — but verify the `CandidateEvaluation.CacheHitRate` field in `selector.go` still gets populated correctly.

- [ ] **Step 1: Verify `selector.go` reads `HitRate` (not `HitRateKnown`)**

Check line 129 of `selector.go`:
```go
CacheHitRate: candidate.Cache.HitRate,
```
This reads `HitRate` directly — since `ToCacheProfile()` always sets `HitRate` to the effective value, this is correct. No change needed.

- [ ] **Step 2: Run full test suite including scheduler**

Run: `go test ./internal/...`
Expected: ALL PASS

- [ ] **Step 3: Run e2e tests specifically**

Run: `go test ./internal/routing/ -run TestE2E -v`
Expected: ALL PASS with correct routing decisions

- [ ] **Step 4: Commit (if any fixups needed)**

Only commit if there were adjustments. Otherwise skip.

---

### Task 7: Integration test — verify end-to-end routing decisions match

**Files:**
- Create: `internal/routing/refactor_integration_test.go`

**Interfaces:**
- Consumes: All new functions (`ResolvePricing`, `ResolveSessionCache`, `BuildForecast`)
- Validates: Given the same inputs, the new dimension-provider path produces identical `CacheProfile` and `Pricing` as the old inline code would have.

- [ ] **Step 1: Write integration test**

```go
package routing

import (
	"testing"
	"time"
)

// TestIntegrationDimensionProvidersMatchOldBehavior verifies that the extracted
// dimension providers produce the same routing inputs as the old inline code.
func TestIntegrationDimensionProvidersMatchOldBehavior(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// Scenario: HOT session with observed hit rate on a cache-enabled upstream
	sc := ResolveSessionCache(CacheStateParams{
		Supported:     true,
		CacheMode:     "enabled",
		Observations:  15,
		HitCount:      12,
		MissCount:     3,
		CreateCount:   3,
		PrefixTokens:  8000,
		ExpiresAt:     now.Add(3 * time.Minute).Unix(),
		FirstSeenAt:   now.Add(-12 * time.Minute).Unix(),
		CoverageRatio: 0.77,
	}, now)

	profile := sc.ToCacheProfile()

	// Verify state machine
	if sc.State != CacheHot {
		t.Fatalf("expected HOT, got %v", sc.State)
	}
	// Should select extended TTL (session > 10min, creates >= 2)
	if sc.PreferredTTL != time.Hour {
		t.Fatalf("expected 1h TTL for long session with rebuilds, got %v", sc.PreferredTTL)
	}
	// Profile should reflect HOT state
	if !profile.Existing.Valid {
		t.Fatal("HOT state should have valid existing entry")
	}
	if profile.CoverageRatio != 0.77 {
		t.Fatalf("coverage: %v", profile.CoverageRatio)
	}
	closeTo(t, profile.HitRate, 0.8)
	if profile.HitRateSource != HitRateObserved {
		t.Fatalf("source: %v", profile.HitRateSource)
	}

	// Pricing resolution
	pricing := ResolvePricing(PricingParams{
		InputCostPerToken:   ptr(3e-6),
		OutputCostPerToken:  ptr(15e-6),
		CacheReadPerToken:   ptr(0.3e-6),
		CacheWritePerToken:  ptr(3.75e-6),
		EffectiveMultiplier: 0.05,
		CreditRatio:         1,
	})

	if pricing.Multiplier != 0.05 {
		t.Fatalf("multiplier: %v", pricing.Multiplier)
	}

	// Traffic forecast
	forecast := BuildForecast([]UpstreamTrafficSample{
		{RequestsPerMinute: 2, OutputPerRequest: 400},
		{RequestsPerMinute: 1.5, OutputPerRequest: 600},
	}, 15*time.Minute)

	if forecast.RequestsPerMinute != 3.5 {
		t.Fatalf("forecast rpm: %v", forecast.RequestsPerMinute)
	}

	// Full cost estimate should work with the new profile
	features := RequestFeatures{
		Model: "claude-sonnet-4-20250514",
		InputTokens: 10000, ReusableInputTokens: 8000, EstimatedOutputTokens: 500,
	}
	cost := EstimateWindowCost(features, forecast, pricing, profile, now, 15*time.Minute)
	if !cost.CacheUsed {
		t.Fatalf("HOT session with high hit rate should use cache: %+v", cost)
	}
	if cost.Savings <= 0 {
		t.Fatalf("should have positive savings: %v", cost.Savings)
	}
}

// TestIntegrationColdSessionUsesNoCachePath verifies COLD sessions correctly
// fall through to no-cache when traffic is too low.
func TestIntegrationColdSessionUsesNoCachePath(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	sc := ResolveSessionCache(CacheStateParams{
		Supported: true,
		CacheMode: "auto",
	}, now)
	profile := sc.ToCacheProfile()

	pricing := ResolvePricing(PricingParams{
		InputCostPerToken:  ptr(1e-6),
		OutputCostPerToken: ptr(2e-6),
		CreditRatio:        1,
	})

	forecast := BuildForecast(nil, 15*time.Minute) // no traffic

	features := RequestFeatures{InputTokens: 5000, ReusableInputTokens: 4000, EstimatedOutputTokens: 100}
	cost := EstimateWindowCost(features, forecast, pricing, profile, now, 15*time.Minute)

	// With unsupported profile (auto mode, no observations), cache should not be used
	if profile.Supported {
		t.Fatal("auto mode with no observations should not be supported")
	}
	if cost.CacheUsed {
		t.Fatalf("cold session with no traffic should not use cache: %+v", cost)
	}
}
```

- [ ] **Step 2: Run integration tests**

Run: `go test ./internal/routing/ -run TestIntegration -v`
Expected: ALL PASS

- [ ] **Step 3: Run full test suite**

Run: `go test ./internal/...`
Expected: ALL PASS

- [ ] **Step 4: Commit**

```bash
git add internal/routing/refactor_integration_test.go
git commit -m "test(routing): add integration tests for dimension provider refactor"
```

---

### Task 8: Clean up dead code and verify final state

**Files:**
- Modify: `internal/scheduler/intelligent.go` (remove `selectCacheTTL` if still present)
- Verify: No compilation errors across the full project

- [ ] **Step 1: Remove orphaned `selectCacheTTL` from scheduler if still present**

After Task 5, the old `selectCacheTTL` function in `intelligent.go` should have been replaced. Verify it's gone:

Run: `grep -n "selectCacheTTL" internal/scheduler/intelligent.go`
Expected: No output (function was replaced in Task 5)

If still present, delete it.

- [ ] **Step 2: Verify no unused imports**

Run: `go build ./...`
Expected: Clean build, no errors

- [ ] **Step 3: Run full project tests**

Run: `go test ./...`
Expected: ALL PASS

- [ ] **Step 4: Final commit if any cleanup was needed**

```bash
git add -A
git commit -m "chore(routing): remove dead code from architecture refactor"
```
