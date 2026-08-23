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
	if profile.HitRateSource != HitRatePrior {
		t.Fatalf("cold supported should use prior: %v", profile.HitRateSource)
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
	if sc.HitRate != 0.8 {
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
		Supported:     true,
		Observations:  5,
		HitCount:      3,
		MissCount:     2,
		CreateCount:   2,
		PrefixTokens:  5000,
		ExpiresAt:     now.Add(-1 * time.Minute).Unix(),
		FirstSeenAt:   now.Add(-20 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:     "enabled",
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
		Supported:     true,
		Observations:  1,
		HitCount:      0,
		MissCount:     0,
		CreateCount:   1,
		PrefixTokens:  5000,
		ExpiresAt:     now.Add(4 * time.Minute).Unix(),
		FirstSeenAt:   now.Add(-30 * time.Second).Unix(),
		CoverageRatio: 1,
		CacheMode:     "enabled",
	}, now)
	if sc.State != CacheWarming {
		t.Fatalf("expected WARMING, got %v", sc.State)
	}
}

func TestAdaptiveTTLSelection(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// Short session, few creates → 5min
	sc := ResolveSessionCache(CacheStateParams{
		Supported:     true,
		Observations:  3,
		CreateCount:   1,
		FirstSeenAt:   now.Add(-3 * time.Minute).Unix(),
		ExpiresAt:     now.Add(2 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:     "enabled",
	}, now)
	if sc.PreferredTTL != 5*time.Minute {
		t.Fatalf("short session should use 5min TTL, got %v", sc.PreferredTTL)
	}

	// Long session + multiple creates → 1h
	sc = ResolveSessionCache(CacheStateParams{
		Supported:     true,
		Observations:  20,
		CreateCount:   3,
		FirstSeenAt:   now.Add(-15 * time.Minute).Unix(),
		ExpiresAt:     now.Add(2 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:     "enabled",
	}, now)
	if sc.PreferredTTL != time.Hour {
		t.Fatalf("long session with rebuilds should use 1h TTL, got %v", sc.PreferredTTL)
	}

	// Sparse requests (interval > 4min) + at least 1 create → 1h
	sc = ResolveSessionCache(CacheStateParams{
		Supported:     true,
		Observations:  2,
		CreateCount:   1,
		FirstSeenAt:   now.Add(-10 * time.Minute).Unix(),
		ExpiresAt:     now.Add(2 * time.Minute).Unix(),
		CoverageRatio: 1,
		CacheMode:     "enabled",
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
