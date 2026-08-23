package routing

import "time"

// UpstreamTrafficSample holds recent traffic statistics for one upstream.
// The scheduler adapter produces these from store.UpstreamRoutingStats.
type UpstreamTrafficSample struct {
	RequestsPerMinute float64
	OutputPerRequest  float64
}

// BuildForecast aggregates traffic samples from all upstreams serving a model
// into a single TrafficForecast. RPM is summed (total model throughput);
// output per request is averaged across upstreams with nonzero traffic.
func BuildForecast(samples []UpstreamTrafficSample, window time.Duration) TrafficForecast {
	if window <= 0 {
		window = 15 * time.Minute
	}

	var totalRPM float64
	var totalOutput float64
	var count int

	for _, s := range samples {
		if s.RequestsPerMinute <= 0 {
			continue
		}
		totalRPM += s.RequestsPerMinute
		totalOutput += s.OutputPerRequest
		count++
	}

	var avgOutput float64
	if count > 0 {
		avgOutput = totalOutput / float64(count)
	}

	return TrafficForecast{
		Window:             window,
		RequestsPerMinute:  totalRPM,
		OutputTokensPerReq: avgOutput,
	}
}
