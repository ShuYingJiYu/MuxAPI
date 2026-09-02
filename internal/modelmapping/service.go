// Package modelmapping provides model name translation and auto-learned
// fallback mappings. It sits between the scheduler and forwarder to translate
// model names per-upstream before requests are sent, and to learn from
// repeated "model unsupported" failures.
package modelmapping

import (
	"strings"
	"sync"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

const (
	// autoLearnThreshold is the number of consecutive failures before an
	// auto-mapping is created.
	autoLearnThreshold = 3

	// autoLearnTTL is how long an auto-learned mapping remains valid before
	// the system re-probes.
	autoLearnTTL = 24 * time.Hour

	// cacheTTL is how long resolved mappings are cached in memory.
	cacheTTL = 30 * time.Second
)

// cacheEntry holds a resolved mapping with expiry.
type cacheEntry struct {
	target  string
	found   bool
	expires time.Time
}

// ModelLister returns the known model list for a specific upstream.
// Used during auto-learning to find date-suffixed variants.
type ModelLister interface {
	UpstreamModels(upstreamID int64) []string
}

// Service manages model name resolution with in-memory caching.
type Service struct {
	store  *store.Store
	lister ModelLister
	mu     sync.RWMutex
	cache  map[cacheKey]cacheEntry
}

type cacheKey struct {
	upstreamID int64
	model      string
}

// New creates a model mapping service backed by the given store.
func New(st *store.Store) *Service {
	return &Service{
		store: st,
		cache: make(map[cacheKey]cacheEntry),
	}
}

// SetModelLister installs the upstream model list provider for auto-learning.
func (s *Service) SetModelLister(lister ModelLister) {
	s.lister = lister
}

// Resolve returns the effective model name for a specific upstream.
// If no mapping exists, returns the original model unchanged.
func (s *Service) Resolve(upstreamID int64, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}

	key := cacheKey{upstreamID: upstreamID, model: model}
	s.mu.RLock()
	if entry, ok := s.cache[key]; ok && time.Now().Before(entry.expires) {
		s.mu.RUnlock()
		if entry.found {
			return entry.target
		}
		return model
	}
	s.mu.RUnlock()

	target, found := s.store.ResolveModelMapping(upstreamID, model)

	s.mu.Lock()
	s.cache[key] = cacheEntry{
		target:  target,
		found:   found,
		expires: time.Now().Add(cacheTTL),
	}
	s.mu.Unlock()

	if found {
		return target
	}
	return model
}

// RecordFailure records a model-unsupported failure for an upstream.
// If the failure count exceeds the threshold, it creates an auto-learned
// mapping to a derived fallback model name.
func (s *Service) RecordFailure(upstreamID int64, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}

	count, err := s.store.IncrementMappingFailure(upstreamID, model)
	if err != nil {
		return
	}

	if count >= autoLearnThreshold {
		fallback := deriveFallback(model)
		if fallback == "" && s.lister != nil {
			fallback = findPrefixMatch(model, s.lister.UpstreamModels(upstreamID))
		}
		if fallback != "" && fallback != model {
			expires := time.Now().Add(autoLearnTTL)
			s.store.UpsertModelMapping(&store.ModelMapping{
				UpstreamID:   upstreamID,
				SourceModel:  model,
				TargetModel:  fallback,
				MappingType:  store.MappingAuto,
				FailureCount: count,
				ExpiresAt:    &expires,
			})
			// Invalidate cache
			s.invalidate(upstreamID, model)
		}
	}
}

// RecordSuccess clears any auto-learned mapping for a model that has been
// confirmed working on an upstream.
func (s *Service) RecordSuccess(upstreamID int64, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	// Only reset failure counters for auto-learned mappings
	existing, err := s.store.GetModelMapping(upstreamID, model)
	if err != nil || existing == nil {
		return
	}
	if existing.MappingType == store.MappingAuto && existing.FailureCount > 0 {
		existing.FailureCount = 0
		existing.TargetModel = ""
		s.store.UpsertModelMapping(existing)
		s.invalidate(upstreamID, model)
	}
}

// InvalidateAll clears the entire in-memory cache.
func (s *Service) InvalidateAll() {
	s.mu.Lock()
	s.cache = make(map[cacheKey]cacheEntry)
	s.mu.Unlock()
}

func (s *Service) invalidate(upstreamID int64, model string) {
	s.mu.Lock()
	delete(s.cache, cacheKey{upstreamID: upstreamID, model: model})
	// Also invalidate global key since resolution checks both
	delete(s.cache, cacheKey{upstreamID: 0, model: model})
	s.mu.Unlock()
}

// deriveFallback generates a candidate fallback model name from a model
// that consistently fails. Strategies:
// 1. Remove date suffix: claude-haiku-4-5-20251001 -> claude-haiku-4-5
// 2. Remove -thinking suffix: claude-opus-4-6-thinking -> claude-opus-4-6
// 3. Add date suffix for short names (common pattern): claude-haiku-4-5 -> claude-haiku-4-5-20251001
func deriveFallback(model string) string {
	// Try removing a date suffix (YYYYMMDD pattern at end)
	if len(model) > 8 {
		suffix := model[len(model)-8:]
		if isAllDigits(suffix) && len(model) > 9 && model[len(model)-9] == '-' {
			return model[:len(model)-9]
		}
	}

	// Try removing -thinking suffix
	if strings.HasSuffix(model, "-thinking") {
		return strings.TrimSuffix(model, "-thinking")
	}

	// Try removing -latest suffix
	if strings.HasSuffix(model, "-latest") {
		return strings.TrimSuffix(model, "-latest")
	}

	// No derivable fallback
	return ""
}

// findPrefixMatch finds a model in the upstream's list that starts with the
// requested model name followed by a separator. For example, requesting
// "claude-haiku-4-5" matches "claude-haiku-4-5-20251001" from the list.
// Prefers the shortest match (most specific) when multiple candidates exist.
func findPrefixMatch(model string, available []string) string {
	var best string
	for _, candidate := range available {
		if candidate == model {
			continue
		}
		if strings.HasPrefix(candidate, model+"-") {
			if best == "" || len(candidate) < len(best) {
				best = candidate
			}
		}
	}
	return best
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
