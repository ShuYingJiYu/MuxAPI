package server

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/store"
)

func (s *Server) persistRoutingAudit(requestID string, started time.Time, groupID int64, model, endpoint string, result forward.Result) {
	features := result.RouteFeatures.Normalize()
	decision := result.RouteDecision
	if decision != nil && decision.SelectedID != 0 {
		record := store.RouteDecisionRecord{
			RequestID: requestID, GroupID: groupID, Model: model, Protocol: features.Protocol,
			Endpoint: endpoint, SessionKey: features.SessionID, PrefixHash: features.CacheKey,
			CacheKey: features.CacheKey, Strategy: "cost", Reason: decision.Reason,
			SelectedUpstreamID: decision.SelectedID, ForecastWindow: decision.Forecast.Window,
			ForecastRequests: decision.Forecast.Requests, EstimatedInputTokens: features.InputTokens,
			ReusablePrefixTokens: features.ReusableInputTokens, EstimatedOutputTokens: features.EstimatedOutputTokens,
			SelectedCost: floatPtr(decision.EffectiveCost), NoCacheCost: floatPtr(decision.Cost.NoCacheTotal),
			CacheCost: floatPtr(decision.Cost.CacheTotal), EstimatedSavings: floatPtr(decision.EstimatedSavings),
			Confidence: decision.Confidence, CacheSelected: decision.Cost.CacheUsed, Exploration: decision.Exploration, CreatedAt: started,
		}
		for _, evaluation := range decision.Evaluations {
			details, _ := json.Marshal(evaluation)
			record.Candidates = append(record.Candidates, store.RouteCandidateRecord{
				UpstreamID: evaluation.CandidateID, UpstreamName: evaluation.CandidateName,
				Protocol: evaluation.Protocol, Priority: evaluation.Priority, Eligible: evaluation.Eligible,
				Selected:        evaluation.CandidateID == decision.SelectedID,
				RejectionReason: evaluation.RejectReason, PricingSource: evaluation.PricingSource,
				PricingConfidence: evaluation.Cost.Confidence, CacheSupported: evaluation.Cost.CacheEligible,
				CacheExisting: evaluation.CacheExisting, CacheSelected: evaluation.Cost.CacheUsed,
				CacheHitRate:      evaluation.CacheHitRate,
				ForecastTotalCost: floatPtr(evaluation.Cost.SelectedTotal), ForecastNoCacheCost: floatPtr(evaluation.Cost.NoCacheTotal),
				ForecastCacheCost: floatPtr(evaluation.Cost.CacheTotal), EstimatedSavings: floatPtr(evaluation.Cost.Savings),
				BreakEvenRequests: floatPtr(evaluation.Cost.BreakEvenRequests), ExpectedHits: evaluation.Cost.ExpectedHits,
				ExpectedMisses: evaluation.Cost.ExpectedMisses, ExpectedCreates: evaluation.Cost.ExpectedCreates,
				EstimatedTTFTMs: evaluation.P95TTFTMs, EstimatedDurationMs: evaluation.P95DurationMs,
				SuccessRate: evaluation.SuccessRate,
				Details:     details,
			})
		}
		if id, err := s.store.SaveRouteDecision(record); err != nil {
			slog.Warn("save route decision failed", "request_id", requestID, "err", err)
		} else {
			_ = id
			complete := store.RouteDecisionOutcome{
				ActualInputTokens: optionalInt64Ptr(result.InputTokens), ActualOutputTokens: optionalInt64Ptr(result.OutputTokens),
				ActualCachedTokens: optionalInt64Ptr(result.CachedTokens), ActualCacheCreationTokens: optionalInt64Ptr(result.CacheCreationTokens),
				Outcome: result.Outcome, CompletedAt: time.Now(),
			}
			if err := s.store.CompleteRouteDecision(requestID, complete); err != nil {
				slog.Warn("complete route decision failed", "request_id", requestID, "err", err)
			}
		}
	}

	observations := make([]store.RoutingObservationRecord, 0, len(result.Attempts))
	for _, attempt := range result.Attempts {
		cacheEligible := (features.ReusableInputTokens > 0 &&
			(attempt.Outcome == forward.OutcomeSuccess || attempt.Outcome == forward.OutcomePartial)) ||
			attempt.CachedTokens > 0 || attempt.CacheCreationTokens > 0
		observations = append(observations, store.RoutingObservationRecord{
			RequestID: requestID, AttemptNo: attempt.AttemptNo, GroupID: groupID, UpstreamID: attempt.UpstreamID,
			APIKeyHash: attempt.UpstreamKeyHash, Model: model, SessionKey: features.SessionID,
			PrefixHash: features.CacheKey, CacheKey: features.CacheKey, PrefixTokens: features.ReusableInputTokens,
			InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens, CachedTokens: attempt.CachedTokens,
			CacheCreationTokens: attempt.CacheCreationTokens, TTFTMs: attempt.TTFTMs, DurationMs: attempt.DurationMs,
			// Routing reliability measures the channel, not malformed client
			// input or a model-capability miss. Only upstream failures and
			// post-commit stream failures lower this rate.
			Success: routingAttemptSucceeded(attempt.Outcome),
			// A reusable prefix with zero cached tokens is an observed miss. Keep
			// it in the denominator so future choices learn the real hit rate.
			CacheEligible: cacheEligible,
			CacheHit:      attempt.CachedTokens > 0, CacheCreated: attempt.CacheCreationTokens > 0,
			CacheTTL:   cacheTTLForProtocol(attempt.Protocol),
			ObservedAt: attempt.CompletedAt,
		})
	}
	if err := s.store.SaveRoutingObservations(observations); err != nil {
		slog.Warn("save routing observations failed", "request_id", requestID, "err", err)
	}
}

func routingAttemptSucceeded(outcome string) bool {
	switch outcome {
	case forward.OutcomeFailed, forward.OutcomePartial:
		return false
	default:
		return true
	}
}

func floatPtr(value float64) *float64 { return &value }

func optionalInt64Ptr(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func cacheTTLForProtocol(protocol string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "gemini":
		// Gemini context caches commonly use a one-hour default lifetime.
		return time.Hour
	case "claude", "anthropic", "openai", "openai-response", "responses", "codex":
		return 5 * time.Minute
	default:
		return 5 * time.Minute
	}
}
