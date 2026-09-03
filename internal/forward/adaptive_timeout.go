package forward

import "time"

// AdaptiveTimeoutParams holds the tuning knobs for per-upstream timeout calculation.
type AdaptiveTimeoutParams struct {
	P95Ms       float64
	Samples     int64
	InputTokens int64
	Multiplier  float64
	Floor       time.Duration
	Ceiling     time.Duration
	MinSamples  int64
	TokenStep   int64
	TokenBonus  time.Duration
	// TokenBonusConfigured distinguishes an explicit zero (disable the bonus)
	// from the zero value used by older callers that expect defaults.
	TokenBonusConfigured bool
}

// ComputeAdaptiveTimeout returns a timeout tailored to the upstream's observed
// latency and the request's input size. Cold-start (few samples) falls back to
// ceiling; otherwise: max(floor, p95 * multiplier + tokenBonus).
func ComputeAdaptiveTimeout(p AdaptiveTimeoutParams) time.Duration {
	if p.Multiplier <= 0 {
		p.Multiplier = 2.0
	}
	if p.Floor <= 0 {
		p.Floor = 10 * time.Second
	}
	if p.Ceiling <= 0 {
		p.Ceiling = 120 * time.Second
	}
	if p.MinSamples <= 0 {
		p.MinSamples = 5
	}
	if p.TokenStep <= 0 {
		p.TokenStep = 50_000
	}
	if !p.TokenBonusConfigured && p.TokenBonus <= 0 {
		p.TokenBonus = 5 * time.Second
	}
	if p.Samples < p.MinSamples {
		return p.Ceiling
	}

	base := time.Duration(p.P95Ms*p.Multiplier) * time.Millisecond

	tokenBonus := time.Duration(p.InputTokens/p.TokenStep) * p.TokenBonus

	timeout := base + tokenBonus
	if timeout < p.Floor {
		timeout = p.Floor
	}
	if timeout > p.Ceiling {
		timeout = p.Ceiling
	}
	return timeout
}
