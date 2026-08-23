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
	Supported     bool
	CacheMode     string // "enabled" / "auto" / "disabled"
	Observations  int64
	HitCount      int64
	MissCount     int64
	CreateCount   int64
	PrefixTokens  int64
	ExpiresAt     int64 // unix seconds, 0 = unknown
	FirstSeenAt   int64 // unix seconds
	LastHitAt     int64 // unix seconds
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

	sc.PreferredTTL = selectAdaptiveTTL(params, now)

	// Determine hit rate
	if params.Observations > 0 && (params.HitCount > 0 || params.MissCount > 0) {
		denom := params.HitCount + params.MissCount
		if denom > 0 {
			sc.HitRate = float64(params.HitCount) / float64(denom)
		}
		sc.HitRateSource = HitRateObserved
	} else if params.CacheMode == "enabled" || (params.Supported && params.Observations == 0) {
		sc.HitRate = defaultPriorHitRate
		sc.HitRateSource = HitRatePrior
	} else {
		sc.HitRateSource = HitRateUnknown
	}

	// Determine expiry
	if params.ExpiresAt > 0 {
		sc.ExpiresAt = time.Unix(params.ExpiresAt, 0)
	}

	// Determine state
	switch {
	case params.Observations == 0:
		sc.State = CacheCold
	case params.ExpiresAt > 0 && params.ExpiresAt <= now.Unix():
		sc.State = CacheExpired
	case params.HitCount == 0 && params.CreateCount > 0 && params.ExpiresAt > now.Unix():
		sc.State = CacheWarming
	case params.ExpiresAt > now.Unix():
		sc.State = CacheHot
	case params.HitCount > 0 || params.CreateCount > 0:
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
	return sc.PreferredTTL > 0 || sc.HitRateSource != HitRateUnknown
}
