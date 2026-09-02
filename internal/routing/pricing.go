package routing

// PricingParams holds the raw inputs needed to resolve pricing for one
// upstream. The caller (scheduler adapter) fetches these from the catalog
// and billing status; this function is a pure deterministic transform.
type PricingParams struct {
	// From LiteLLM catalog (nil = unknown)
	InputCostPerToken  *float64
	OutputCostPerToken *float64
	CacheReadPerToken  *float64
	CacheWritePerToken *float64

	// From billing status (0 = not available at this tier)
	EffectiveMultiplier float64
	GroupMultiplier     float64
	LastKnownMultiplier float64

	// Per-upstream config
	CreditRatio float64
}

// ResolvePricing assembles a Pricing struct from catalog rates and the
// multiplier fallback chain. The three-tier multiplier resolution is:
//  1. EffectiveMultiplier (provider-reported, per-upstream)
//  2. GroupMultiplier (provider-reported, per-group)
//  3. LastKnownMultiplier (stale snapshot fallback)
//  4. Default 1.0
//
// Each tier divides by CreditRatio when > 0.
func ResolvePricing(params PricingParams) Pricing {
	result := Pricing{Source: "catalog", Confidence: 0.55, Multiplier: 1}

	if params.InputCostPerToken != nil {
		result.InputPerToken = *params.InputCostPerToken
		result.InputKnown = true
	}
	if params.OutputCostPerToken != nil {
		result.OutputPerToken = *params.OutputCostPerToken
		result.OutputKnown = true
	}
	if params.CacheReadPerToken != nil {
		result.CacheReadPerToken = *params.CacheReadPerToken
		result.CacheReadKnown = true
	}
	if params.CacheWritePerToken != nil {
		result.CacheWritePerToken = *params.CacheWritePerToken
		result.CacheWriteKnown = true
	}

	creditRatio := params.CreditRatio
	if creditRatio <= 0 {
		creditRatio = 1
	}

	switch {
	case params.EffectiveMultiplier > 0:
		result.Multiplier = params.EffectiveMultiplier / creditRatio
		result.Source = "catalog+provider-billing"
		result.Confidence = 0.8
	case params.GroupMultiplier > 0:
		result.Multiplier = params.GroupMultiplier / creditRatio
		result.Source = "catalog+provider-billing-group"
		result.Confidence = 0.7
	case params.LastKnownMultiplier > 0:
		result.Multiplier = params.LastKnownMultiplier / creditRatio
		result.Source = "catalog+provider-billing-stale"
		result.Confidence = 0.6
	}

	return result
}
