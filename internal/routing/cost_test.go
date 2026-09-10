package routing

import (
	"math"
	"testing"
	"time"
)

func TestEstimateWindowCostNoCache(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800, EstimatedOutputTokens: 100}
	price := Pricing{InputPerToken: 1e-6, OutputPerToken: 2e-6, InputKnown: true, OutputKnown: true, Multiplier: 1}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 10, Window: 15 * time.Minute}, price, CacheProfile{}, time.Time{}, 15*time.Minute)

	closeTo(t, cost.SelectedTotal, 0.012)
	closeTo(t, cost.NoCacheInputCost, 0.010)
	closeTo(t, cost.OutputCost, 0.002)
	if cost.CacheUsed || cost.CacheEligible {
		t.Fatalf("cache unexpectedly used: %+v", cost)
	}
}

func TestEstimateWindowCostCacheWinsAtVolume(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800, EstimatedOutputTokens: 100}
	price := Pricing{
		InputPerToken: 1e-6, OutputPerToken: 2e-6,
		CacheWritePerToken: 1.25e-6, CacheReadPerToken: 0.1e-6,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
		Multiplier: 1, Confidence: 0.9,
	}
	cache := CacheProfile{Supported: true, TTL: 15 * time.Minute, HitRate: 1, HitRateSource: HitRateObserved}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 10, Window: 15 * time.Minute}, price, cache, time.Time{}, 15*time.Minute)

	if !cost.CacheUsed || !cost.CacheEligible {
		t.Fatalf("cache should win: %+v", cost)
	}
	closeTo(t, cost.ExpectedCreates, 1)
	closeTo(t, cost.ExpectedHits, 9)
	closeTo(t, cost.CacheInputCost, 0.002)
	closeTo(t, cost.CacheWriteCost, 0.001)
	closeTo(t, cost.CacheReadCost, 0.00072)
	closeTo(t, cost.SelectedTotal, 0.00572)
	closeTo(t, cost.Savings, 0.00628)
	if cost.BreakEvenRequests <= 1 || cost.BreakEvenRequests >= 2 {
		t.Fatalf("break-even = %v, want between 1 and 2", cost.BreakEvenRequests)
	}
}

func TestEstimateWindowCostChoosesOrdinaryWhenWriteIsTooExpensive(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 900, EstimatedOutputTokens: 10}
	price := Pricing{
		InputPerToken: 1e-6, OutputPerToken: 2e-6,
		CacheWritePerToken: 3e-6, CacheReadPerToken: 0.1e-6,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 1}, price,
		CacheProfile{Supported: true, HitRate: 1, HitRateSource: HitRateObserved}, time.Time{}, 5*time.Minute)

	if cost.CacheUsed {
		t.Fatalf("one expensive cache write should not win: %+v", cost)
	}
	if cost.CacheTotal <= cost.NoCacheTotal {
		t.Fatalf("cache total %v should exceed no-cache %v", cost.CacheTotal, cost.NoCacheTotal)
	}
	closeTo(t, cost.SelectedTotal, cost.NoCacheTotal)
}

func TestEstimateWindowCostDefaultsMissingCacheRates(t *testing.T) {
	features := RequestFeatures{InputTokens: 2_000, ReusableInputTokens: 1_800, EstimatedOutputTokens: 100}
	price := Pricing{InputPerToken: 1e-6, OutputPerToken: 2e-6, InputKnown: true, OutputKnown: true, Confidence: 0.8}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 20, Window: 15 * time.Minute}, price,
		CacheProfile{Supported: true, HitRate: 0.9, HitRateSource: HitRateObserved}, time.Time{}, 15*time.Minute)
	if !cost.PricingComplete || len(cost.Warnings) == 0 {
		t.Fatalf("missing cache rates should use an auditable default: %+v", cost)
	}
	if !cost.CacheUsed || cost.CacheTotal >= cost.NoCacheTotal {
		t.Fatalf("default cache rates should win this repeated workload: %+v", cost)
	}
}

func TestEstimateWindowCostAccountsForTTLLifetimes(t *testing.T) {
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800}
	price := Pricing{
		InputPerToken: 1, OutputPerToken: 0, CacheWritePerToken: 1.25, CacheReadPerToken: 0.1,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 10, Window: 12 * time.Minute}, price,
		CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 1, HitRateSource: HitRateObserved}, time.Time{}, 15*time.Minute)

	closeTo(t, cost.CacheLifetimes, 3)
	closeTo(t, cost.ExpectedCreates, 3)
	closeTo(t, cost.ExpectedHits, 7)
}

func TestExistingCacheAvoidsInitialWrite(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	features := RequestFeatures{InputTokens: 1_000, ReusableInputTokens: 800}
	price := Pricing{
		InputPerToken: 1, OutputPerToken: 0, CacheWritePerToken: 2, CacheReadPerToken: 0.1,
		InputKnown: true, OutputKnown: true, CacheWriteKnown: true, CacheReadKnown: true,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 2, Window: time.Minute}, price,
		CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 1, HitRateSource: HitRateObserved,
			Existing: CacheEntry{Valid: true, ExpiresAt: now.Add(2 * time.Minute)}}, now, 15*time.Minute)

	closeTo(t, cost.ExpectedCreates, 0)
	closeTo(t, cost.ExpectedHits, 2)
}

func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10*math.Max(1, math.Abs(want)) {
		t.Fatalf("got %.12g, want %.12g", got, want)
	}
}

// Upstream-injected inflation tokens (system prompts, tool schemas) are stable
// per session and share the provider cache lifecycle, so they must be priced
// as cache_read on hits — not as full input suffix. This regression guards
// against pre-fix behaviour where inflation flowed into "suffix" and was
// billed at pricing.InputPerToken.
func TestEstimateWindowCostInflationBilledAsCacheRead(t *testing.T) {
	features := RequestFeatures{InputTokens: 100_000, ReusableInputTokens: 100_000, EstimatedOutputTokens: 100}
	price := Pricing{
		InputPerToken: 5e-6, OutputPerToken: 25e-6,
		CacheReadPerToken: 5e-7, CacheWritePerToken: 6.25e-6,
		InputKnown: true, OutputKnown: true, CacheReadKnown: true, CacheWriteKnown: true,
		Multiplier: 1,
	}
	// InputInflation=1.8 means upstream really bills 180k input tokens.
	// Under cache with hit_rate=1 and coverage=1, ALL 180k should flow through
	// cache_read (0.5e-7), and the extra 80k must NOT show up at 5e-6 in suffix.
	cache := CacheProfile{
		Supported: true, TTL: 5 * time.Minute,
		HitRate: 1, HitRateSource: HitRateObserved,
		CoverageRatio: 1, InputInflation: 1.8,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 100, Window: 5 * time.Minute}, price, cache, time.Time{}, 5*time.Minute)

	if !cost.CacheEligible {
		t.Fatalf("cache should be eligible: %+v", cost)
	}
	// Original InputTokens == ReusableInputTokens, so true suffix is 0. The
	// cache branch must contain no full-price input; the 80k inflation goes
	// through cache_read.
	if cost.CacheInputCost > 0.001 {
		t.Fatalf("inflation must not appear as full-price suffix; CacheInputCost=%.6f", cost.CacheInputCost)
	}
	// With one guaranteed miss per lifetime and 99 hits, cacheable=180k:
	expectedRead := 99 * 180_000.0 * 5e-7
	if math.Abs(cost.CacheReadCost-expectedRead) > 1e-6 {
		t.Fatalf("CacheReadCost=%.6f want ~%.6f", cost.CacheReadCost, expectedRead)
	}
	expectedWrite := 1 * 180_000.0 * 6.25e-6
	if math.Abs(cost.CacheWriteCost-expectedWrite) > 1e-6 {
		t.Fatalf("CacheWriteCost=%.6f want ~%.6f", cost.CacheWriteCost, expectedWrite)
	}
	// no_cache branch unchanged: 100 * inflated 180k * 5e-6 = 90.
	if math.Abs(cost.NoCacheTotal-90.25) > 0.01 {
		t.Fatalf("NoCacheTotal=%.4f want ~90.25", cost.NoCacheTotal)
	}
	if !cost.CacheUsed || cost.CacheTotal >= cost.NoCacheTotal {
		t.Fatalf("cache should win over no-cache for high-inflation upstream: %+v", cost)
	}
}

// The true suffix (newly appended user turn) must still be billed at the full
// input rate. Only the *inflation* portion moves to cache pricing.
func TestEstimateWindowCostTrueSuffixStillFullPrice(t *testing.T) {
	// User-visible payload: 100k prefix + 5k new user turn = 105k. Inflation 1.5
	// adds 52.5k more tokens the upstream injects.
	features := RequestFeatures{InputTokens: 105_000, ReusableInputTokens: 100_000, EstimatedOutputTokens: 50}
	price := Pricing{
		InputPerToken: 5e-6, OutputPerToken: 25e-6,
		CacheReadPerToken: 5e-7, CacheWritePerToken: 6.25e-6,
		InputKnown: true, OutputKnown: true, CacheReadKnown: true, CacheWriteKnown: true,
		Multiplier: 1,
	}
	cache := CacheProfile{
		Supported: true, TTL: 5 * time.Minute,
		HitRate: 1, HitRateSource: HitRateObserved,
		CoverageRatio: 1, InputInflation: 1.5,
	}
	cost := EstimateWindowCost(features, TrafficForecast{Requests: 10, Window: 5 * time.Minute}, price, cache, time.Time{}, 5*time.Minute)

	// True suffix = 5_000 tokens per request at full price: 10 * 5000 * 5e-6 = 0.25.
	if math.Abs(cost.CacheInputCost-0.25) > 1e-9 {
		t.Fatalf("true suffix must be at full input price; got CacheInputCost=%.6f want 0.25", cost.CacheInputCost)
	}
	// Cacheable = 100_000 + 52_500 = 152_500.
	// hits = 9, misses = 1 (one guaranteed miss for lifetime).
	if math.Abs(cost.CacheReadCost-0.68625) > 1e-6 {
		t.Fatalf("CacheReadCost=%.6f want 0.68625", cost.CacheReadCost)
	}
	if math.Abs(cost.CacheWriteCost-0.953125) > 1e-6 {
		t.Fatalf("CacheWriteCost=%.6f want 0.953125", cost.CacheWriteCost)
	}
}
