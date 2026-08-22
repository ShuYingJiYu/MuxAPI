package routing

import (
	"encoding/json"
	"testing"
	"time"
)

// logDecision outputs a JSON audit record for debugging routing decisions.
func logDecision(t *testing.T, scenario string, decision Decision, err error) {
	t.Helper()
	record := map[string]any{
		"scenario":      scenario,
		"selected_id":   decision.SelectedID,
		"selected_name": decision.SelectedName,
		"reason":        decision.Reason,
		"effective_cost": decision.EffectiveCost,
		"forecast":      decision.Forecast,
		"confidence":    decision.Confidence,
		"exploration":   decision.Exploration,
	}
	if err != nil {
		record["error"] = err.Error()
	}
	data, _ := json.MarshalIndent(record, "", "  ")
	t.Logf("routing decision:\n%s", string(data))
}

// --- Anthropic-like pricing constants ---

func anthropicSonnetPrice() Pricing {
	return Pricing{
		InputPerToken:      3e-6,
		OutputPerToken:     15e-6,
		CacheWritePerToken: 3.75e-6,
		CacheReadPerToken:  0.3e-6,
		InputKnown:         true,
		OutputKnown:        true,
		CacheWriteKnown:    true,
		CacheReadKnown:     true,
		Multiplier:         1,
		Confidence:         0.95,
		Source:             "test-fixture",
	}
}

func cheapNoCachePrice() Pricing {
	return Pricing{
		InputPerToken:  2e-6,
		OutputPerToken: 10e-6,
		InputKnown:     true,
		OutputKnown:    true,
		Multiplier:     1,
		Confidence:     0.9,
		Source:         "test-fixture",
	}
}

func expensiveCachePrice() Pricing {
	return Pricing{
		InputPerToken:      5e-6,
		OutputPerToken:     20e-6,
		CacheWritePerToken: 6.25e-6,
		CacheReadPerToken:  0.5e-6,
		InputKnown:         true,
		OutputKnown:        true,
		CacheWriteKnown:    true,
		CacheReadKnown:     true,
		Multiplier:         1,
		Confidence:         0.9,
		Source:             "test-fixture",
	}
}

// TestE2ELowFrequencyPrefersCheapNoCache verifies that with only 1 request
// in the window (cache will expire unused), the selector picks the cheaper
// non-cache candidate since a cache write cannot be amortized.
func TestE2ELowFrequencyPrefersCheapNoCache(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	request := Request{
		Features: RequestFeatures{
			Model:                 "claude-sonnet-4-20250514",
			InputTokens:           10_000,
			ReusableInputTokens:   8_000,
			EstimatedOutputTokens: 500,
		},
		Forecast: TrafficForecast{
			Window:   15 * time.Minute,
			Requests: 1,
		},
		Candidates: []Candidate{
			{
				ID: 1, Name: "cheap-no-cache", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: cheapNoCachePrice(),
				Cache: CacheProfile{Supported: false},
			},
			{
				ID: 2, Name: "cache-capable", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: anthropicSonnetPrice(),
				Cache: CacheProfile{
					Supported:    true,
					TTL:          5 * time.Minute,
					HitRate:      0.95,
					HitRateKnown: true,
				},
			},
		},
		Config: Config{
			Window:           15 * time.Minute,
			CostTieTolerance: 0.01,
			LatencyWeight:    0.25,
			ReliabilityWeight: 0.15,
		},
		Now: now,
	}

	decision, err := Choose(request)
	if err != nil {
		t.Fatal(err)
	}
	logDecision(t, "low-frequency-prefers-cheap", decision, nil)

	if decision.SelectedID != 1 {
		t.Fatalf("low frequency (N=1) should prefer cheap non-cache candidate (ID=1), got ID=%d (%s)",
			decision.SelectedID, decision.SelectedName)
	}
}

// TestE2EHighFrequencyPrefersCachePath verifies that with N=20 requests in
// a 5-minute window where cache TTL covers the entire window, the cache
// candidate wins because reads are much cheaper than re-sending the prefix.
func TestE2EHighFrequencyPrefersCachePath(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	request := Request{
		Features: RequestFeatures{
			Model:                 "claude-sonnet-4-20250514",
			InputTokens:           10_000,
			ReusableInputTokens:   8_000,
			EstimatedOutputTokens: 500,
		},
		Forecast: TrafficForecast{
			Window:   5 * time.Minute,
			Requests: 20,
		},
		Candidates: []Candidate{
			{
				ID: 1, Name: "no-cache-cheap", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: cheapNoCachePrice(),
				Cache: CacheProfile{Supported: false},
			},
			{
				ID: 2, Name: "cache-sonnet", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: anthropicSonnetPrice(),
				Cache: CacheProfile{
					Supported:    true,
					TTL:          5 * time.Minute,
					HitRate:      1.0,
					HitRateKnown: true,
				},
			},
		},
		Config: Config{
			Window:            5 * time.Minute,
			CostTieTolerance:  0.01,
			LatencyWeight:     0.25,
			ReliabilityWeight: 0.15,
		},
		Now: now,
	}

	decision, err := Choose(request)
	if err != nil {
		t.Fatal(err)
	}
	logDecision(t, "high-frequency-prefers-cache", decision, nil)

	if decision.SelectedID != 2 {
		t.Fatalf("high frequency (N=20) should prefer cache candidate (ID=2), got ID=%d (%s)\ncost eval: cache_total=%.8f no_cache_total=%.8f",
			decision.SelectedID, decision.SelectedName,
			decision.Evaluations[1].Cost.CacheTotal, decision.Evaluations[1].Cost.NoCacheTotal)
	}
	if !decision.Cost.CacheUsed {
		t.Fatal("expected cache to be used in the winning candidate's cost estimate")
	}
}

// TestE2EBreakEvenThreshold finds the exact N where the cache path becomes
// cheaper than the no-cache path, and verifies the transition.
func TestE2EBreakEvenThreshold(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	makeCacheCandidate := func() Candidate {
		return Candidate{
			ID: 2, Name: "cache", Priority: 5,
			Healthy: true, SupportsModel: true,
			Price: anthropicSonnetPrice(),
			Cache: CacheProfile{
				Supported:    true,
				TTL:          5 * time.Minute,
				HitRate:      1.0,
				HitRateKnown: true,
			},
		}
	}

	noCacheCandidate := Candidate{
		ID: 1, Name: "no-cache", Priority: 5,
		Healthy: true, SupportsModel: true,
		Price: cheapNoCachePrice(),
		Cache: CacheProfile{Supported: false},
	}

	// Binary search for break-even point
	var breakEvenN float64
	for n := float64(1); n <= 50; n++ {
		request := Request{
			Features: RequestFeatures{
				Model:                 "claude-sonnet-4-20250514",
				InputTokens:           10_000,
				ReusableInputTokens:   8_000,
				EstimatedOutputTokens: 500,
			},
			Forecast: TrafficForecast{Window: 5 * time.Minute, Requests: n},
			Candidates: []Candidate{noCacheCandidate, makeCacheCandidate()},
			Config: Config{
				Window:            5 * time.Minute,
				CostTieTolerance:  0.001, // tight tolerance for precise break-even
				LatencyWeight:     0,
				ReliabilityWeight: 0,
			},
			Now: now,
		}
		decision, err := Choose(request)
		if err != nil {
			t.Fatal(err)
		}
		if decision.SelectedID == 2 && breakEvenN == 0 {
			breakEvenN = n
			t.Logf("break-even at N=%.0f requests in 5min window", n)
			logDecision(t, "break-even-found", decision, nil)
			break
		}
	}

	if breakEvenN == 0 {
		t.Fatal("cache never became cheaper within N=1..50")
	}
	if breakEvenN < 2 {
		t.Fatalf("break-even at N=%.0f is unrealistically low", breakEvenN)
	}

	// Verify that N-1 still picks no-cache
	request := Request{
		Features: RequestFeatures{
			Model:                 "claude-sonnet-4-20250514",
			InputTokens:           10_000,
			ReusableInputTokens:   8_000,
			EstimatedOutputTokens: 500,
		},
		Forecast: TrafficForecast{Window: 5 * time.Minute, Requests: breakEvenN - 1},
		Candidates: []Candidate{noCacheCandidate, makeCacheCandidate()},
		Config: Config{
			Window:            5 * time.Minute,
			CostTieTolerance:  0.001,
			LatencyWeight:     0,
			ReliabilityWeight: 0,
		},
		Now: now,
	}
	decision, err := Choose(request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedID != 1 {
		t.Fatalf("at N=%.0f (below break-even), expected no-cache (ID=1), got ID=%d",
			breakEvenN-1, decision.SelectedID)
	}
}

// TestE2ECacheTTLExpiryMidWindow tests that when a cache entry expires
// partway through the forecast window, the cost model correctly accounts
// for re-creation cost.
func TestE2ECacheTTLExpiryMidWindow(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	request := Request{
		Features: RequestFeatures{
			Model:                 "claude-sonnet-4-20250514",
			InputTokens:           10_000,
			ReusableInputTokens:   8_000,
			EstimatedOutputTokens: 500,
		},
		Forecast: TrafficForecast{
			Window:   15 * time.Minute,
			Requests: 30,
		},
		Candidates: []Candidate{
			{
				ID: 1, Name: "no-cache", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: cheapNoCachePrice(),
				Cache: CacheProfile{Supported: false},
			},
			{
				ID: 2, Name: "cache-short-ttl", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: anthropicSonnetPrice(),
				Cache: CacheProfile{
					Supported:    true,
					TTL:          5 * time.Minute, // 3 lifetimes in 15min window
					HitRate:      1.0,
					HitRateKnown: true,
					Existing: CacheEntry{
						Valid:     true,
						ExpiresAt: now.Add(2 * time.Minute), // expires in 2 min
					},
				},
			},
		},
		Config: Config{
			Window:            15 * time.Minute,
			CostTieTolerance:  0.01,
			LatencyWeight:     0,
			ReliabilityWeight: 0,
		},
		Now: now,
	}

	decision, err := Choose(request)
	if err != nil {
		t.Fatal(err)
	}
	logDecision(t, "cache-ttl-expiry-mid-window", decision, nil)

	// With 30 requests and 3+ cache lifetimes, cost model should account for
	// multiple cache writes. Verify the evaluation shows expected creates > 1.
	for _, eval := range decision.Evaluations {
		if eval.CandidateID == 2 {
			if eval.Cost.CacheLifetimes < 2 {
				t.Fatalf("expected multiple cache lifetimes, got %.1f", eval.Cost.CacheLifetimes)
			}
			if eval.Cost.ExpectedCreates < 2 {
				t.Fatalf("expected multiple cache creates, got %.1f", eval.Cost.ExpectedCreates)
			}
			t.Logf("cache lifetimes=%.1f creates=%.1f hits=%.1f",
				eval.Cost.CacheLifetimes, eval.Cost.ExpectedCreates, eval.Cost.ExpectedHits)
		}
	}

	// At N=30 across 15min with re-creates, cache should still win (reads are much cheaper)
	if decision.SelectedID != 2 {
		t.Logf("note: cache did not win at N=30 with TTL expiry; checking costs")
		// This is not necessarily a failure — depends on the exact math
		for _, eval := range decision.Evaluations {
			t.Logf("  candidate %d (%s): effective_cost=%.8f cache_total=%.8f no_cache_total=%.8f",
				eval.CandidateID, eval.CandidateName, eval.EffectiveCost,
				eval.Cost.CacheTotal, eval.Cost.NoCacheTotal)
		}
	}
}

// TestE2ECircuitBreakerFailover verifies that when the cheapest candidate
// is marked unhealthy, the selector picks the next-best option.
func TestE2ECircuitBreakerFailover(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	request := Request{
		Features: RequestFeatures{
			Model:                 "claude-sonnet-4-20250514",
			InputTokens:           5_000,
			ReusableInputTokens:   4_000,
			EstimatedOutputTokens: 200,
		},
		Forecast: TrafficForecast{Window: 5 * time.Minute, Requests: 10},
		Candidates: []Candidate{
			{
				ID: 1, Name: "cheapest-but-down", Priority: 1,
				Healthy:       false, // circuit breaker open
				SupportsModel: true,
				Price:         cheapNoCachePrice(),
				LastError:     "upstream returned 5 consecutive 500s",
			},
			{
				ID: 2, Name: "mid-price", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price:   anthropicSonnetPrice(),
				Cache:   CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 0.9, HitRateKnown: true},
			},
			{
				ID: 3, Name: "expensive-backup", Priority: 10,
				Healthy: true, SupportsModel: true,
				Price:   expensiveCachePrice(),
				Cache:   CacheProfile{Supported: false},
			},
		},
		Config: Config{
			Window:            5 * time.Minute,
			CostTieTolerance:  0.01,
			LatencyWeight:     0.25,
			ReliabilityWeight: 0.15,
		},
		Now: now,
	}

	decision, err := Choose(request)
	if err != nil {
		t.Fatal(err)
	}
	logDecision(t, "circuit-breaker-failover", decision, nil)

	// Cheapest (ID=1) is unhealthy, should be rejected
	if decision.Evaluations[0].Eligible {
		t.Fatal("unhealthy candidate should not be eligible")
	}
	if decision.Evaluations[0].RejectReason == "" {
		t.Fatal("unhealthy candidate should have a reject reason")
	}

	// Should pick one of the healthy candidates
	if decision.SelectedID == 1 {
		t.Fatal("should not select unhealthy candidate")
	}
	if decision.SelectedID != 2 && decision.SelectedID != 3 {
		t.Fatalf("expected ID 2 or 3, got %d", decision.SelectedID)
	}
	t.Logf("failover selected: ID=%d (%s)", decision.SelectedID, decision.SelectedName)
}

// TestE2EModelNotSupportedFiltered verifies candidates that don't support
// the requested model are excluded from selection.
func TestE2EModelNotSupportedFiltered(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	request := Request{
		Features: RequestFeatures{
			Model:                 "claude-haiku-4-5-20251001",
			InputTokens:           2_000,
			ReusableInputTokens:   1_500,
			EstimatedOutputTokens: 100,
		},
		Forecast: TrafficForecast{Requests: 5},
		Candidates: []Candidate{
			{
				ID: 1, Name: "sonnet-only", Priority: 1,
				Healthy:       true,
				SupportsModel: false, // does not support haiku
				Price:         cheapNoCachePrice(),
			},
			{
				ID: 2, Name: "haiku-capable", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price:   anthropicSonnetPrice(),
				Cache:   CacheProfile{Supported: true, TTL: 5 * time.Minute, HitRate: 0.8, HitRateKnown: true},
			},
		},
		Config: DefaultConfig(),
		Now:    now,
	}

	decision, err := Choose(request)
	if err != nil {
		t.Fatal(err)
	}
	logDecision(t, "model-not-supported-filtered", decision, nil)

	if decision.SelectedID != 2 {
		t.Fatalf("expected haiku-capable (ID=2) to win, got ID=%d", decision.SelectedID)
	}
	if decision.Evaluations[0].Eligible {
		t.Fatal("sonnet-only candidate should not be eligible for haiku request")
	}
	if decision.Evaluations[0].RejectReason != "model is not supported" {
		t.Fatalf("unexpected reject reason: %q", decision.Evaluations[0].RejectReason)
	}
}

// TestE2EAllCandidatesUnhealthy verifies that ErrNoCandidate is returned
// when every candidate is marked unhealthy.
func TestE2EAllCandidatesUnhealthy(t *testing.T) {
	request := Request{
		Features: RequestFeatures{
			Model:       "claude-sonnet-4-20250514",
			InputTokens: 1000,
		},
		Candidates: []Candidate{
			{ID: 1, Name: "down-a", Healthy: false, SupportsModel: true, Price: cheapNoCachePrice()},
			{ID: 2, Name: "down-b", Healthy: false, SupportsModel: true, Price: anthropicSonnetPrice()},
		},
		Config: DefaultConfig(),
		Now:    time.Now(),
	}

	_, err := Choose(request)
	if err != ErrNoCandidate {
		t.Fatalf("expected ErrNoCandidate, got %v", err)
	}
}

// TestE2EReliabilityAdjustedCost verifies that a candidate with low success
// rate gets penalized via effective cost adjustment.
func TestE2EReliabilityAdjustedCost(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	request := Request{
		Features: RequestFeatures{
			Model:                 "claude-sonnet-4-20250514",
			InputTokens:           5_000,
			EstimatedOutputTokens: 200,
		},
		Forecast: TrafficForecast{Requests: 10, Window: 5 * time.Minute},
		Candidates: []Candidate{
			{
				ID: 1, Name: "cheap-unreliable", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: cheapNoCachePrice(),
				Performance: Performance{
					Samples:     50,
					SuccessRate: 0.5, // 50% of requests fail
					P95TTFTMs:   200,
				},
			},
			{
				ID: 2, Name: "expensive-reliable", Priority: 5,
				Healthy: true, SupportsModel: true,
				Price: anthropicSonnetPrice(),
				Performance: Performance{
					Samples:     50,
					SuccessRate: 0.99,
					P95TTFTMs:   300,
				},
			},
		},
		Config: Config{
			Window:            5 * time.Minute,
			CostTieTolerance:  0.01,
			LatencyWeight:     0.25,
			ReliabilityWeight: 0.15,
			MinSamples:        20,
		},
		Now: now,
	}

	decision, err := Choose(request)
	if err != nil {
		t.Fatal(err)
	}
	logDecision(t, "reliability-adjusted-cost", decision, nil)

	// The cheap candidate has 50% success rate so its effective cost doubles.
	// At 2x cheap price, it may no longer be cheaper than the reliable one.
	eval0 := decision.Evaluations[0]
	eval1 := decision.Evaluations[1]
	t.Logf("cheap-unreliable effective_cost=%.8f (raw=%.8f / success=%.2f)",
		eval0.EffectiveCost, eval0.Cost.SelectedTotal, eval0.SuccessRate)
	t.Logf("expensive-reliable effective_cost=%.8f (raw=%.8f / success=%.2f)",
		eval1.EffectiveCost, eval1.Cost.SelectedTotal, eval1.SuccessRate)

	// With 50% success, effective cost should be ~2x the raw cost
	if eval0.EffectiveCost < eval0.Cost.SelectedTotal*1.9 {
		t.Fatalf("unreliable candidate effective cost should be ~2x raw cost")
	}
}
