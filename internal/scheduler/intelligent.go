package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// intelligentRouter adapts the durable store/health observations to the
// protocol-independent routing cost model. The short-lived read-through
// caches reduce database work on the request hot path; PostgreSQL remains the
// source of truth and the existing health manager supplies fast breaker state.
type intelligentRouter struct {
	store   *store.Store
	config  func() routing.Config
	mu      sync.Mutex
	billing billingCacheEntry
	prices  map[string]priceCacheEntry
	stats   map[statsCacheKey]statsCacheEntry
	prefix  map[prefixCacheKey]prefixCacheEntry
}

const (
	billingCacheTTL = 30 * time.Second
	routingCacheTTL = 5 * time.Second
	maxPrefixCache  = 10_000
)

type billingCacheEntry struct {
	expires time.Time
	values  map[int64]store.BillingStatus
}

type priceCacheEntry struct {
	expires time.Time
	value   store.ModelPricing
	err     error
}

type statsCacheKey struct {
	upstreamID int64
	model      string
	window     time.Duration
}

type statsCacheEntry struct {
	expires time.Time
	value   store.UpstreamRoutingStats
	err     error
}

type prefixCacheKey struct {
	apiKeyHash string
	upstreamID int64
	model      string
	prefixHash string
	window     time.Duration
}

type prefixCacheEntry struct {
	expires time.Time
	value   store.PrefixCacheStats
	err     error
}

func (r *intelligentRouter) pick(s *Scheduler, groupID int64, model string, features routing.RequestFeatures, exclude map[int64]bool) (*upstream.Upstream, routing.Decision, error) {
	all := s.list(groupID)
	if len(all) == 0 {
		return nil, routing.Decision{}, ErrNoUpstream
	}
	blocked := make(map[int64]bool, len(exclude))
	for id, value := range exclude {
		blocked[id] = value
	}
	now := time.Now()
	cfg := routing.DefaultConfig()
	if r.config != nil {
		cfg = r.config()
	}
	for attempts := 0; attempts < len(all); attempts++ {
		candidates := r.candidates(s, all, model, features, blocked, now, cfg)
		if len(candidates) == 0 {
			break
		}
		decision, err := routing.Choose(routing.Request{
			Features: features, Forecast: r.forecast(all, model, features, cfg, now),
			Candidates: candidates, Config: cfg, Now: now,
		})
		if err != nil {
			break
		}
		for _, candidate := range all {
			if candidate.ID != decision.SelectedID || blocked[candidate.ID] {
				continue
			}
			if s.health.Claim(candidate.ID, model) {
				return candidate, decision, nil
			}
			blocked[candidate.ID] = true
			break
		}
	}
	// Unknown price, a cold catalog, or a claim race must never make the
	// gateway unavailable. Preserve the original scheduler behavior.
	candidate, err := s.PickExcluding(groupID, model, blocked)
	if candidate == nil || err != nil {
		return candidate, routing.Decision{}, err
	}
	return candidate, routing.Decision{
		SelectedID: candidate.ID, SelectedName: candidate.Name,
		Reason:   "fallback: cost data incomplete; standard health/P2C scheduler",
		Forecast: routing.TrafficForecast{Window: cfg.Window},
	}, nil
}

func (r *intelligentRouter) candidates(s *Scheduler, all []*upstream.Upstream, model string, features routing.RequestFeatures, blocked map[int64]bool, now time.Time, cfg routing.Config) []routing.Candidate {
	statuses := r.billingStatuses(now)
	out := make([]routing.Candidate, 0, len(all))
	for _, item := range all {
		if item == nil || blocked[item.ID] {
			continue
		}
		state := ""
		if reporter, ok := s.health.(interface{ EffectiveState(int64) string }); ok {
			state = reporter.EffectiveState(item.ID)
		}
		healthy := state != "OPEN"
		supports := s.health.IsAvailable(item.ID, model)
		price := r.price(item, model, statuses)
		performance := r.performance(item.ID, model, cfg.Window, now, s.health)
		cache := r.cache(item, model, features, now, cfg.Window)
		out = append(out, routing.Candidate{
			ID: item.ID, Name: item.Name, Protocol: item.Protocol,
			Priority: item.Priority, Weight: item.Weight, Healthy: healthy,
			SupportsModel: supports, Price: price, Cache: cache,
			Performance: performance,
			LastError:   state,
		})
	}
	return out
}

func (r *intelligentRouter) price(item *upstream.Upstream, model string, statuses map[int64]store.BillingStatus) routing.Pricing {
	price, err := r.modelPricing(model, time.Now())
	if err != nil {
		return routing.Pricing{}
	}
	result := routing.Pricing{Source: "LiteLLM", Confidence: 0.55, Multiplier: 1}
	if price.InputCostPerToken != nil {
		result.InputPerToken, result.InputKnown = *price.InputCostPerToken, true
	}
	if price.OutputCostPerToken != nil {
		result.OutputPerToken, result.OutputKnown = *price.OutputCostPerToken, true
	}
	if price.CacheReadInputTokenCost != nil {
		result.CacheReadPerToken, result.CacheReadKnown = *price.CacheReadInputTokenCost, true
	}
	if price.CacheWriteInputTokenCost != nil {
		result.CacheWritePerToken, result.CacheWriteKnown = *price.CacheWriteInputTokenCost, true
	}
	if status, ok := statuses[item.ID]; ok {
		if status.EffectiveMultiplier != nil && *status.EffectiveMultiplier > 0 {
			multiplier := *status.EffectiveMultiplier
			if item.CreditRatio > 0 {
				multiplier /= item.CreditRatio
			}
			result.Multiplier = multiplier
			result.Source = "LiteLLM+provider-billing"
			result.Confidence = 0.8
		} else if status.GroupMultiplier != nil && *status.GroupMultiplier > 0 {
			multiplier := *status.GroupMultiplier
			if item.CreditRatio > 0 {
				multiplier /= item.CreditRatio
			}
			result.Multiplier = multiplier
			result.Source = "LiteLLM+provider-billing-group"
			result.Confidence = 0.7
		}
	}
	return result
}

func (r *intelligentRouter) performance(id int64, model string, window time.Duration, now time.Time, h Health) routing.Performance {
	stats, err := r.upstreamStats(id, model, window, now)
	if err != nil {
		return routing.Performance{InFlight: h.InFlight(id), Samples: 0}
	}
	samples := stats.Requests
	if samples == 0 {
		// The in-memory EWMA survives the transition before durable routing
		// observations accumulate, but it is not a reliability sample.
		latency := float64(h.LatencyEWMA(id))
		return routing.Performance{P50TTFTMs: latency, P95TTFTMs: latency, InFlight: h.InFlight(id)}
	}
	return routing.Performance{
		Samples: samples, P50TTFTMs: float64(stats.P50TTFTMs),
		P95TTFTMs: float64(stats.P95TTFTMs), P95DurationMs: float64(stats.P95DurationMs),
		SuccessRate: stats.SuccessRate, InFlight: h.InFlight(id),
	}
}

func (r *intelligentRouter) cache(item *upstream.Upstream, model string, features routing.RequestFeatures, now time.Time, window time.Duration) routing.CacheProfile {
	if features.ReusableInputTokens <= 0 {
		return routing.CacheProfile{}
	}
	cacheMode, _ := upstream.NormalizeCacheMode(item.CacheMode)
	if cacheMode == upstream.CacheDisabled {
		return routing.CacheProfile{}
	}
	keyHash := hashCredential(item.APIKey)
	// Use SessionID for prefix stats lookup when available. In multi-turn
	// conversations (like Claude Code), each request adds content so the
	// exact prefix hash changes every turn — but the session's cache behavior
	// is stable: hits accumulate across turns because the provider caches the
	// growing prefix. Using session_key lets the hit rate observation persist.
	prefixHash := features.CacheKey
	if features.SessionID != "" && features.SessionID != features.CacheKey {
		prefixHash = features.SessionID
	}
	stats, err := r.prefixStats(keyHash, item.ID, model, prefixHash, window, now)
	observed := err == nil
	// A row containing only misses is useful for the hit-rate denominator, but
	// does not prove that the provider actually supports prompt caching.
	cacheObserved := observed && (stats.HitCount > 0 || stats.CreateCount > 0)
	supported := cacheMode == upstream.CacheEnabled || cacheObserved
	if !supported {
		return routing.CacheProfile{}
	}
	ttl := selectCacheTTL(stats, now)
	if strings.EqualFold(strings.TrimSpace(item.Protocol), "gemini") {
		ttl = time.Hour
	}
	profile := routing.CacheProfile{
		Supported: supported, TTL: ttl, MinTokens: 1024,
		HitRateKnown:   observed && stats.Observations > 0,
		DefaultHitRate: 0.85,
		Existing:       routing.CacheEntry{Valid: observed && stats.Valid, PrefixTokens: stats.PrefixTokens},
		PreferredTTL:   ttl,
	}
	if observed {
		profile.HitRate = stats.WindowHitRate
		if stats.ExpiresAt > 0 {
			profile.Existing.ExpiresAt = time.Unix(stats.ExpiresAt, 0)
		}
	}
	return profile
}

// selectCacheTTL picks an adaptive TTL based on observed session behavior.
// Sessions that keep losing their cache (multiple creates over a long period)
// benefit from requesting longer provider TTLs; infrequent requesters may also
// benefit when the break-even math works out.
func selectCacheTTL(stats store.PrefixCacheStats, now time.Time) time.Duration {
	const defaultTTL = 5 * time.Minute
	const extendedTTL = time.Hour

	if stats.FirstSeenAt <= 0 || stats.Observations <= 0 {
		return defaultTTL
	}
	sessionDuration := time.Duration(now.Unix()-stats.FirstSeenAt) * time.Second

	// If session has been running > 10min and cache was rebuilt >= 2 times,
	// the session keeps losing cache — extend TTL to reduce rebuilds.
	if sessionDuration > 10*time.Minute && stats.CreateCount >= 2 {
		return extendedTTL
	}

	// If average request interval > 4min, evaluate whether extended TTL
	// breaks even: a 1h TTL covers ~15 requests at 4min intervals vs
	// rebuilding every 5min. Worth it if we'd otherwise create more than once.
	avgInterval := sessionDuration / time.Duration(stats.Observations)
	if avgInterval > 4*time.Minute && stats.CreateCount >= 1 {
		return extendedTTL
	}

	return defaultTTL
}

func (r *intelligentRouter) forecast(all []*upstream.Upstream, model string, features routing.RequestFeatures, cfg routing.Config, now time.Time) routing.TrafficForecast {
	// Aggregate over all upstreams so a channel with no own history does not
	// receive a zero-volume forecast merely because it is newly configured.
	var requestsPerMinute, outputPerRequest float64
	var count int
	for _, item := range all {
		if item == nil {
			continue
		}
		stats, err := r.upstreamStats(item.ID, model, cfg.Window, now)
		if err != nil || stats.Requests == 0 {
			continue
		}
		requestsPerMinute += stats.RequestsPerMinute
		outputPerRequest += stats.OutputPerRequest
		count++
	}
	if count > 0 {
		outputPerRequest /= float64(count)
	}
	return routing.TrafficForecast{Window: cfg.Window, RequestsPerMinute: requestsPerMinute, OutputTokensPerReq: outputPerRequest}
}

func hashCredential(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (r *intelligentRouter) billingStatuses(now time.Time) map[int64]store.BillingStatus {
	r.mu.Lock()
	if r.billing.values != nil && now.Before(r.billing.expires) {
		values := r.billing.values
		r.mu.Unlock()
		return values
	}
	r.mu.Unlock()
	values, err := r.store.ListBillingStatuses()
	if err != nil {
		return nil
	}
	r.mu.Lock()
	r.billing = billingCacheEntry{expires: now.Add(billingCacheTTL), values: values}
	r.mu.Unlock()
	return values
}

func (r *intelligentRouter) modelPricing(model string, now time.Time) (store.ModelPricing, error) {
	key := strings.TrimSpace(model)
	r.mu.Lock()
	if r.prices != nil {
		if entry, ok := r.prices[key]; ok && now.Before(entry.expires) {
			r.mu.Unlock()
			return entry.value, entry.err
		}
	}
	r.mu.Unlock()
	value, err := r.store.LookupModelPricing(key)
	ttl := time.Hour
	if err != nil {
		// The pricing manager may populate the catalog shortly after startup;
		// do not pin a cold-start miss for the whole successful-price TTL.
		ttl = routingCacheTTL
	}
	r.mu.Lock()
	if r.prices == nil {
		r.prices = make(map[string]priceCacheEntry)
	}
	r.prices[key] = priceCacheEntry{expires: now.Add(ttl), value: value, err: err}
	r.mu.Unlock()
	return value, err
}

func (r *intelligentRouter) upstreamStats(id int64, model string, window time.Duration, now time.Time) (store.UpstreamRoutingStats, error) {
	key := statsCacheKey{upstreamID: id, model: model, window: window}
	r.mu.Lock()
	if entry, ok := r.stats[key]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.value, entry.err
	}
	r.mu.Unlock()
	value, err := r.store.GetUpstreamRoutingStats(id, model, window, now)
	r.mu.Lock()
	if r.stats == nil {
		r.stats = make(map[statsCacheKey]statsCacheEntry)
	}
	r.stats[key] = statsCacheEntry{expires: now.Add(routingCacheTTL), value: value, err: err}
	r.mu.Unlock()
	return value, err
}

func (r *intelligentRouter) prefixStats(apiKeyHash string, upstreamID int64, model, prefixHash string, window time.Duration, now time.Time) (store.PrefixCacheStats, error) {
	key := prefixCacheKey{apiKeyHash: apiKeyHash, upstreamID: upstreamID, model: model, prefixHash: prefixHash, window: window}
	r.mu.Lock()
	if entry, ok := r.prefix[key]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.value, entry.err
	}
	r.mu.Unlock()
	value, err := r.store.GetPrefixCacheStats(apiKeyHash, upstreamID, model, prefixHash, window, now)
	r.mu.Lock()
	if r.prefix == nil {
		r.prefix = make(map[prefixCacheKey]prefixCacheEntry)
	}
	if len(r.prefix) >= maxPrefixCache {
		r.prefix = make(map[prefixCacheKey]prefixCacheEntry)
	}
	r.prefix[key] = prefixCacheEntry{expires: now.Add(routingCacheTTL), value: value, err: err}
	r.mu.Unlock()
	return value, err
}
