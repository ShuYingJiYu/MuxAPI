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
	const minSamples = 5
	if p.Samples < minSamples {
		return p.Ceiling
	}

	base := time.Duration(p.P95Ms*p.Multiplier) * time.Millisecond

	// Every 50k input tokens adds 5s tolerance.
	tokenBonus := time.Duration(p.InputTokens/50000) * 5 * time.Second

	timeout := base + tokenBonus
	if timeout < p.Floor {
		timeout = p.Floor
	}
	if timeout > p.Ceiling {
		timeout = p.Ceiling
	}
	return timeout
}
