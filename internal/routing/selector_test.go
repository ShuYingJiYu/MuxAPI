package routing

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func basePrice(input float64) Pricing {
	return Pricing{InputPerToken: input, OutputPerToken: 1e-6, InputKnown: true, OutputKnown: true, Multiplier: 1, Confidence: 0.8}
}

func healthyCandidate(id int64, name string, priority int, price Pricing) Candidate {
	return Candidate{ID: id, Name: name, Priority: priority, Healthy: true, SupportsModel: true, Price: price}
}

func TestChooseCrossesPriorityForLowerCost(t *testing.T) {
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000, EstimatedOutputTokens: 100},
		Forecast: TrafficForecast{Requests: 1},
		Candidates: []Candidate{
			healthyCandidate(1, "priority expensive", 0, basePrice(5e-6)),
			healthyCandidate(2, "lower priority cheap", 10, basePrice(1e-6)),
		},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != 2 {
		t.Fatalf("selected %d, want lower-priority cheaper candidate 2; %+v", decision.SelectedID, decision)
	}
	if decision.EstimatedSavings <= 0 || !strings.Contains(decision.Reason, "lowest forecast cost") {
		t.Fatalf("missing cost explanation: %+v", decision)
	}
}

func TestChooseCacheCandidateAtRepeatedVolume(t *testing.T) {
	cachePrice := basePrice(2e-6)
	cachePrice.CacheWritePerToken = 2.5e-6
	cachePrice.CacheReadPerToken = 0.2e-6
	cachePrice.CacheWriteKnown = true
	cachePrice.CacheReadKnown = true
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000, ReusableInputTokens: 9_000, EstimatedOutputTokens: 100},
		Forecast: TrafficForecast{Requests: 20, Window: 5 * time.Minute},
		Candidates: []Candidate{
			healthyCandidate(1, "no cache", 0, basePrice(1e-6)),
			func() Candidate {
				c := healthyCandidate(2, "cache", 5, cachePrice)
				c.Cache = CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 1, HitRateKnown: true}
				return c
			}(),
		},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != 2 || !decision.Cost.CacheUsed {
		t.Fatalf("cache candidate should win repeated workload: %+v", decision)
	}
}

func TestChooseNearEqualCostUsesLatency(t *testing.T) {
	fast := healthyCandidate(1, "fast", 10, basePrice(1.005e-6))
	fast.Performance = Performance{Samples: 100, SuccessRate: 1, P95TTFTMs: 100}
	slow := healthyCandidate(2, "slow", 0, basePrice(1e-6))
	slow.Performance = Performance{Samples: 100, SuccessRate: 1, P95TTFTMs: 1_000}
	cfg := DefaultConfig()
	cfg.CostTieTolerance = 0.01
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000}, Forecast: TrafficForecast{Requests: 1},
		Candidates: []Candidate{slow, fast}, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != fast.ID || !strings.Contains(decision.Reason, "latency") {
		t.Fatalf("near-equal cost should prefer faster candidate: %+v", decision)
	}
}

func TestChooseRejectsUnknownPriceAndHardLatency(t *testing.T) {
	unknown := healthyCandidate(1, "unknown", 0, Pricing{})
	slow := healthyCandidate(2, "slow", 0, basePrice(1e-6))
	slow.Performance = Performance{Samples: 10, SuccessRate: 1, P95TTFTMs: 2_000}
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 100}, Candidates: []Candidate{unknown, slow},
		Config: Config{MaxTTFTMs: 1_000},
	})
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("error = %v, want ErrNoCandidate", err)
	}
	if decision.Evaluations[0].RejectReason == "" || decision.Evaluations[1].RejectReason == "" {
		t.Fatalf("missing rejection explanations: %+v", decision.Evaluations)
	}
}

func TestChooseAccountsForFailureRetryCost(t *testing.T) {
	unreliable := healthyCandidate(1, "cheap unreliable", 0, basePrice(0.6e-6))
	unreliable.Performance = Performance{Samples: 100, SuccessRate: 0.5, P95TTFTMs: 100}
	reliable := healthyCandidate(2, "reliable", 0, basePrice(1e-6))
	reliable.Performance = Performance{Samples: 100, SuccessRate: 1, P95TTFTMs: 200}
	decision, err := Choose(Request{
		Features: RequestFeatures{InputTokens: 10_000}, Candidates: []Candidate{unreliable, reliable},
		Config: DefaultConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != reliable.ID {
		t.Fatalf("expected retry-adjusted reliable candidate, got %+v", decision)
	}
}

func TestSelectorWrapperPick(t *testing.T) {
	selector := NewSelector(DefaultConfig())
	selected, decision, err := selector.Pick(Request{
		Features:   RequestFeatures{InputTokens: 100},
		Candidates: []Candidate{healthyCandidate(7, "only", 0, basePrice(1e-6))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != 7 || decision.SelectedID != 7 {
		t.Fatalf("wrapper selected %+v with decision %+v", selected, decision)
	}
}

func TestChooseExploresLeastObservedEligibleCandidate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cold := healthyCandidate(2, "cold", 10, basePrice(2e-6))
	cold.Performance.Samples = 0
	warm := healthyCandidate(1, "warm", 0, basePrice(1e-6))
	warm.Performance = Performance{Samples: 100, SuccessRate: 1}
	cfg := DefaultConfig()
	cfg.ExplorationRate = 1
	decision, err := Choose(Request{
		Features:   RequestFeatures{Model: "gpt-5", CacheKey: "session"},
		Candidates: []Candidate{warm, cold}, Config: cfg, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != cold.ID || !decision.Exploration {
		t.Fatalf("expected exploration sample of cold candidate: %+v", decision)
	}
}
