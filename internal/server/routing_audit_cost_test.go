package server

import (
	"math"
	"testing"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/tokenusage"
)

func TestRouteActualCostUsesSelectedPricingSnapshot(t *testing.T) {
	decision := &routing.Decision{
		SelectedID: 64,
		Evaluations: []routing.CandidateEvaluation{{
			CandidateID: 64,
			Pricing: routing.Pricing{
				InputPerToken: 5e-6, OutputPerToken: 25e-6,
				CacheReadPerToken: 5e-7, CacheWritePerToken: 6.25e-6,
				InputKnown: true, OutputKnown: true, CacheReadKnown: true,
				CacheWriteKnown: true, Multiplier: 0.2,
			},
		}},
	}
	result := forward.Result{
		FinalUpstreamID: 64, InputTokens: 49_801, OutputTokens: 10,
		CachedTokens: 284_384, CacheCreationTokens: 7_039,
	}
	cost, ok := routeActualCost(decision, result)
	if !ok || math.Abs(cost-0.08708815) > 1e-9 {
		t.Fatalf("actual cost=%.9f ok=%v want 0.08708815", cost, ok)
	}
}

func TestRouteCostDoesNotTreatMalformedUsageAsReliable(t *testing.T) {
	decision := &routing.Decision{SelectedID: 1, Evaluations: []routing.CandidateEvaluation{{CandidateID: 1, Pricing: routing.Pricing{InputKnown: true, OutputKnown: true, InputPerToken: 1, OutputPerToken: 1}}}}
	for _, quality := range []string{"inconsistent", "unclassified"} {
		result := forward.Result{FinalUpstreamID: 1, InputTokens: 100, TokenAccounting: tokenusage.New(tokenusage.Estimate{}, tokenusage.Report{Present: true, Quality: quality})}
		if _, ok := routeActualCost(decision, result); ok {
			t.Fatalf("%s usage produced an authoritative cost", quality)
		}
	}
}
