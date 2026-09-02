package routing

import "testing"

func TestResolvePricingFromCatalog(t *testing.T) {
	p := ResolvePricing(PricingParams{
		InputCostPerToken:  ptr(3e-6),
		OutputCostPerToken: ptr(15e-6),
		CacheReadPerToken:  ptr(0.3e-6),
		CacheWritePerToken: ptr(3.75e-6),
		CreditRatio:        1,
	})
	if !p.InputKnown || p.InputPerToken != 3e-6 {
		t.Fatalf("input: %+v", p)
	}
	if !p.OutputKnown || p.OutputPerToken != 15e-6 {
		t.Fatalf("output: %+v", p)
	}
	if !p.CacheReadKnown || p.CacheReadPerToken != 0.3e-6 {
		t.Fatalf("cache read: %+v", p)
	}
	if p.Multiplier != 1 {
		t.Fatalf("multiplier: %+v", p)
	}
	if p.Source != "catalog" {
		t.Fatalf("source: %+v", p)
	}
	if p.Confidence != 0.55 {
		t.Fatalf("confidence: %+v", p)
	}
}

func TestResolvePricingMultiplierFallbackChain(t *testing.T) {
	// EffectiveMultiplier wins
	p := ResolvePricing(PricingParams{
		InputCostPerToken:   ptr(1e-6),
		OutputCostPerToken:  ptr(1e-6),
		EffectiveMultiplier: 0.05,
		GroupMultiplier:     0.1,
		LastKnownMultiplier: 0.2,
		CreditRatio:         2,
	})
	if p.Multiplier != 0.025 {
		t.Fatalf("effective multiplier: got %v", p.Multiplier)
	}
	if p.Source != "catalog+provider-billing" || p.Confidence != 0.8 {
		t.Fatalf("source/confidence: %+v", p)
	}

	// GroupMultiplier fallback
	p = ResolvePricing(PricingParams{
		InputCostPerToken:   ptr(1e-6),
		OutputCostPerToken:  ptr(1e-6),
		EffectiveMultiplier: 0,
		GroupMultiplier:     0.1,
		LastKnownMultiplier: 0.2,
		CreditRatio:         2,
	})
	if p.Multiplier != 0.05 {
		t.Fatalf("group multiplier: got %v", p.Multiplier)
	}
	if p.Source != "catalog+provider-billing-group" || p.Confidence != 0.7 {
		t.Fatalf("source/confidence: %+v", p)
	}

	// LastKnown fallback
	p = ResolvePricing(PricingParams{
		InputCostPerToken:   ptr(1e-6),
		OutputCostPerToken:  ptr(1e-6),
		EffectiveMultiplier: 0,
		GroupMultiplier:     0,
		LastKnownMultiplier: 0.2,
		CreditRatio:         2,
	})
	if p.Multiplier != 0.1 {
		t.Fatalf("last known multiplier: got %v", p.Multiplier)
	}
	if p.Source != "catalog+provider-billing-stale" || p.Confidence != 0.6 {
		t.Fatalf("source/confidence: %+v", p)
	}

	// No multiplier info → default 1
	p = ResolvePricing(PricingParams{
		InputCostPerToken:  ptr(1e-6),
		OutputCostPerToken: ptr(1e-6),
		CreditRatio:        1,
	})
	if p.Multiplier != 1 {
		t.Fatalf("default multiplier: got %v", p.Multiplier)
	}
}

func TestResolvePricingNilCatalog(t *testing.T) {
	p := ResolvePricing(PricingParams{})
	if p.InputKnown || p.OutputKnown {
		t.Fatalf("nil catalog should produce unknown pricing: %+v", p)
	}
}

func ptr(v float64) *float64 { return &v }
