package server

import (
	"math"
	"testing"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/routing"
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

// A P2C fallback used to return an empty Decision. Without an evaluation for
// the selected candidate routeActualCost cannot resolve a Pricing, which left
// actual_cost NULL on ~23% of production route_decisions.
func TestRouteActualCostRequiresEvaluationForSelectedUpstream(t *testing.T) {
	decision := &routing.Decision{
		SelectedID: 64,
		Evaluations: []routing.CandidateEvaluation{{
			CandidateID: 99,
			Pricing:     routing.Pricing{InputPerToken: 5e-6, InputKnown: true},
		}},
	}
	if _, ok := routeActualCost(decision, forward.Result{FinalUpstreamID: 64, InputTokens: 10}); ok {
		t.Fatal("a decision without an evaluation for the selected upstream must not report a cost")
	}
}

func TestDecisionStrategyLabelsFallbacks(t *testing.T) {
	cases := []struct {
		name     string
		decision *routing.Decision
		want     string
	}{
		{name: "cost chosen", decision: &routing.Decision{SelectedID: 4, Reason: "cheapest eligible"}, want: "cost"},
		{name: "p2c fallback", decision: &routing.Decision{SelectedID: 4, Reason: "fallback: cost data incomplete; standard health/P2C scheduler"}, want: "p2c-fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionStrategy(tc.decision); got != tc.want {
				t.Fatalf("strategy=%q want %q", got, tc.want)
			}
		})
	}
}
