package routing

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Observation is the normalized usage/performance event emitted after an
// upstream attempt. Cache tokens use the provider's native meaning: cached is
// cache-read input and cache-creation is cache-write input.
type Observation struct {
	At                  time.Time `json:"at"`
	UpstreamID          int64     `json:"upstream_id"`
	Model               string    `json:"model"`
	Protocol            string    `json:"protocol,omitempty"`
	CacheKey            string    `json:"cache_key,omitempty"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CachedTokens        int64     `json:"cached_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	TTFTMs              float64   `json:"ttft_ms"`
	DurationMs          float64   `json:"duration_ms"`
	Success             bool      `json:"success"`
}

// TrafficStats is a snapshot over one key/model/upstream dimension.
type TrafficStats struct {
	FromAt              time.Time `json:"from_at"`
	ToAt                time.Time `json:"to_at"`
	Requests            int64     `json:"requests"`
	Successes           int64     `json:"successes"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CachedTokens        int64     `json:"cached_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	CacheRate           float64   `json:"cache_rate"`
	HitRate             float64   `json:"hit_rate"`
	RequestsPerMinute   float64   `json:"requests_per_minute"`
	OutputPerRequest    float64   `json:"output_per_request"`
	P50TTFTMs           float64   `json:"p50_ttft_ms"`
	P95TTFTMs           float64   `json:"p95_ttft_ms"`
	P95DurationMs       float64   `json:"p95_duration_ms"`
	SuccessRate         float64   `json:"success_rate"`
	Samples             int64     `json:"samples"`
}

// WindowStats pairs an aggregate with its requested horizon.
type WindowStats struct {
	Window time.Duration `json:"window"`
	Stats  TrafficStats  `json:"stats"`
}

// Tracker collects a bounded in-memory recent history. The durable request
// audit remains the source of truth; this component is intentionally cheap and
// can be rebuilt from the database after restart.
type Tracker struct {
	mu       sync.RWMutex
	window   time.Duration
	maxItems int
	items    []Observation
}

// NewTracker creates a tracker. A non-positive window defaults to 15 minutes;
// maxItems <= 0 uses 100,000 observations.
func NewTracker(window time.Duration, maxItems int) *Tracker {
	if window <= 0 {
		window = 15 * time.Minute
	}
	if maxItems <= 0 {
		maxItems = 100_000
	}
	return &Tracker{window: window, maxItems: maxItems, items: make([]Observation, 0, min(maxItems, 1024))}
}

// Observe records one attempt. Zero timestamps are replaced with now, and
// negative counters are clamped before storage.
func (t *Tracker) Observe(observation Observation) {
	if t == nil {
		return
	}
	if observation.At.IsZero() {
		observation.At = time.Now()
	}
	if observation.InputTokens < 0 {
		observation.InputTokens = 0
	}
	if observation.OutputTokens < 0 {
		observation.OutputTokens = 0
	}
	if observation.CachedTokens < 0 {
		observation.CachedTokens = 0
	}
	if observation.CacheCreationTokens < 0 {
		observation.CacheCreationTokens = 0
	}
	observation.Model = strings.TrimSpace(observation.Model)
	observation.CacheKey = strings.TrimSpace(observation.CacheKey)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items = append(t.items, observation)
	if len(t.items) > t.maxItems {
		start := len(t.items) - t.maxItems
		t.items = append([]Observation(nil), t.items[start:]...)
	}
}

// Snapshot computes stats at now. An ID/model/key of zero/empty is a wildcard.
func (t *Tracker) Snapshot(now time.Time, upstreamID int64, model, cacheKey string) TrafficStats {
	window := time.Duration(0)
	if t != nil {
		window = t.window
	}
	return t.SnapshotWindow(now, window, upstreamID, model, cacheKey)
}

// SnapshotWindow computes the same aggregate over an explicit horizon. The
// tracker does not discard by age, so callers can query 1m/5m/15m/1h views
// from one bounded observation buffer.
func (t *Tracker) SnapshotWindow(now time.Time, window time.Duration, upstreamID int64, model, cacheKey string) TrafficStats {
	if t == nil {
		return TrafficStats{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	if window <= 0 {
		window = t.window
	}
	from := now.Add(-window)
	model = strings.TrimSpace(model)
	cacheKey = strings.TrimSpace(cacheKey)
	t.mu.RLock()
	defer t.mu.RUnlock()
	var selected []Observation
	for _, item := range t.items {
		if item.At.Before(from) || item.At.After(now) {
			continue
		}
		if upstreamID != 0 && item.UpstreamID != upstreamID {
			continue
		}
		if model != "" && item.Model != model {
			continue
		}
		if cacheKey != "" && item.CacheKey != cacheKey {
			continue
		}
		selected = append(selected, item)
	}
	stats := TrafficStats{FromAt: from, ToAt: now, Requests: int64(len(selected))}
	if len(selected) == 0 {
		return stats
	}
	ttfts := make([]float64, 0, len(selected))
	durations := make([]float64, 0, len(selected))
	cacheInputTokens := int64(0)
	for _, item := range selected {
		stats.InputTokens += item.InputTokens
		stats.OutputTokens += item.OutputTokens
		stats.CachedTokens += item.CachedTokens
		stats.CacheCreationTokens += item.CacheCreationTokens
		cacheInputTokens += item.InputTokens
		if NormalizeProtocol(item.Protocol) == "claude" {
			cacheInputTokens += item.CachedTokens + item.CacheCreationTokens
		}
		if item.Success {
			stats.Successes++
		}
		if item.TTFTMs > 0 {
			ttfts = append(ttfts, item.TTFTMs)
		}
		if item.DurationMs > 0 {
			durations = append(durations, item.DurationMs)
		}
	}
	minutes := now.Sub(from).Minutes()
	if minutes <= 0 {
		minutes = window.Minutes()
	}
	stats.RequestsPerMinute = float64(stats.Requests) / minutes
	stats.OutputPerRequest = float64(stats.OutputTokens) / float64(stats.Requests)
	stats.CacheRate = ratio(float64(stats.CachedTokens), float64(cacheInputTokens))
	// Hit rate is defined over requests with a cache signal. Missing usage is
	// excluded rather than counted as a miss.
	cacheRequests := 0
	hits := 0
	for _, item := range selected {
		if item.CachedTokens > 0 || item.CacheCreationTokens > 0 {
			cacheRequests++
			if item.CachedTokens > 0 && item.CacheCreationTokens == 0 {
				hits++
			}
		}
	}
	stats.HitRate = ratio(float64(hits), float64(cacheRequests))
	stats.SuccessRate = ratio(float64(stats.Successes), float64(stats.Requests))
	stats.P50TTFTMs = percentile(ttfts, 0.50)
	stats.P95TTFTMs = percentile(ttfts, 0.95)
	stats.P95DurationMs = percentile(durations, 0.95)
	stats.Samples = int64(len(selected))
	return stats
}

// MultiWindowSnapshot returns the standard 1m/5m/15m/1h views when windows is
// empty, or the caller's positive durations in the supplied order.
func (t *Tracker) MultiWindowSnapshot(now time.Time, windows []time.Duration, upstreamID int64, model, cacheKey string) []WindowStats {
	if len(windows) == 0 {
		windows = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	}
	out := make([]WindowStats, 0, len(windows))
	for _, window := range windows {
		if window <= 0 {
			continue
		}
		out = append(out, WindowStats{Window: window, Stats: t.SnapshotWindow(now, window, upstreamID, model, cacheKey)})
	}
	return out
}

// Forecast converts recent stats to the cost-model input. A caller may supply
// a different horizon while retaining the observed per-minute rate.
func (t *Tracker) Forecast(now time.Time, horizon time.Duration, upstreamID int64, model, cacheKey string) TrafficForecast {
	stats := t.Snapshot(now, upstreamID, model, cacheKey)
	if horizon <= 0 {
		if t != nil {
			horizon = t.window
		} else {
			horizon = 15 * time.Minute
		}
	}
	return TrafficForecast{Window: horizon, RequestsPerMinute: stats.RequestsPerMinute, OutputTokensPerReq: stats.OutputPerRequest}
}

func ratio(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	return numerator / denominator
}

func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	if fraction <= 0 {
		return values[0]
	}
	if fraction >= 1 {
		return values[len(values)-1]
	}
	index := int(float64(len(values)-1) * fraction)
	return values[index]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
