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

	if sc.State != CacheHot {
		t.Fatalf("expected HOT, got %v", sc.State)
	}
	// Should select extended TTL (session > 10min, creates >= 2)
	if sc.PreferredTTL != time.Hour {
		t.Fatalf("expected 1h TTL for long session with rebuilds, got %v", sc.PreferredTTL)
	}
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

	// Full cost estimate with new profile
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

// TestIntegrationColdSessionFallback verifies COLD sessions with no traffic
// produce a non-cache profile.
func TestIntegrationColdSessionFallback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	sc := ResolveSessionCache(CacheStateParams{
		Supported: false,
	}, now)
	profile := sc.ToCacheProfile()

	if profile.Supported {
		t.Fatal("unsupported should produce unsupported profile")
	}

	pricing := ResolvePricing(PricingParams{
		InputCostPerToken:  ptr(1e-6),
		OutputCostPerToken: ptr(2e-6),
		CreditRatio:        1,
	})

	forecast := BuildForecast(nil, 15*time.Minute)

	features := RequestFeatures{InputTokens: 5000, ReusableInputTokens: 4000, EstimatedOutputTokens: 100}
	cost := EstimateWindowCost(features, forecast, pricing, profile, now, 15*time.Minute)
	if cost.CacheUsed {
		t.Fatalf("cold unsupported session should not use cache: %+v", cost)
	}
}

// TestIntegrationStateTransitions verifies the state machine transitions.
func TestIntegrationStateTransitions(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// COLD → first request arrives, cache created → WARMING
	warming := ResolveSessionCache(CacheStateParams{
		Supported:    true,
		CacheMode:    "enabled",
		Observations: 1,
		CreateCount:  1,
		PrefixTokens: 5000,
		ExpiresAt:    now.Add(5 * time.Minute).Unix(),
		FirstSeenAt:  now.Unix(),
	}, now)
	if warming.State != CacheWarming {
		t.Fatalf("expected WARMING, got %v", warming.State)
	}

	// WARMING → hit observed → HOT
	hot := ResolveSessionCache(CacheStateParams{
		Supported:    true,
		CacheMode:    "enabled",
		Observations: 5,
		HitCount:     3,
		MissCount:    1,
		CreateCount:  1,
		PrefixTokens: 5000,
		ExpiresAt:    now.Add(3 * time.Minute).Unix(),
		FirstSeenAt:  now.Add(-3 * time.Minute).Unix(),
	}, now)
	if hot.State != CacheHot {
		t.Fatalf("expected HOT, got %v", hot.State)
	}

	// HOT → TTL passes → EXPIRED
	expired := ResolveSessionCache(CacheStateParams{
		Supported:    true,
		CacheMode:    "enabled",
		Observations: 10,
		HitCount:     7,
		MissCount:    3,
		CreateCount:  2,
		PrefixTokens: 5000,
		ExpiresAt:    now.Add(-30 * time.Second).Unix(),
		FirstSeenAt:  now.Add(-10 * time.Minute).Unix(),
	}, now)
	if expired.State != CacheExpired {
		t.Fatalf("expected EXPIRED, got %v", expired.State)
	}

	// Verify profiles differ in Existing.Valid
	if !warming.ToCacheProfile().Existing.Valid {
		t.Fatal("WARMING should have valid existing")
	}
	if !hot.ToCacheProfile().Existing.Valid {
		t.Fatal("HOT should have valid existing")
	}
	if expired.ToCacheProfile().Existing.Valid {
		t.Fatal("EXPIRED should NOT have valid existing")
	}
}
