package routing

import (
	"testing"
	"time"
)

func TestBuildForecastAggregatesRPM(t *testing.T) {
	f := BuildForecast([]UpstreamTrafficSample{
		{RequestsPerMinute: 2.0, OutputPerRequest: 500},
		{RequestsPerMinute: 3.0, OutputPerRequest: 300},
	}, 15*time.Minute)

	if f.Window != 15*time.Minute {
		t.Fatalf("window: %v", f.Window)
	}
	if f.RequestsPerMinute != 5.0 {
		t.Fatalf("rpm: %v", f.RequestsPerMinute)
	}
	if f.OutputTokensPerReq != 400 {
		t.Fatalf("output per req: %v", f.OutputTokensPerReq)
	}
}

func TestBuildForecastEmpty(t *testing.T) {
	f := BuildForecast(nil, 15*time.Minute)
	if f.RequestsPerMinute != 0 || f.OutputTokensPerReq != 0 {
		t.Fatalf("empty should be zero: %+v", f)
	}
	if f.Window != 15*time.Minute {
		t.Fatalf("window should still be set: %v", f.Window)
	}
}

func TestBuildForecastSkipsZeroRPM(t *testing.T) {
	f := BuildForecast([]UpstreamTrafficSample{
		{RequestsPerMinute: 0, OutputPerRequest: 500},
		{RequestsPerMinute: 4.0, OutputPerRequest: 200},
	}, 5*time.Minute)

	if f.RequestsPerMinute != 4.0 {
		t.Fatalf("should skip zero rpm: %v", f.RequestsPerMinute)
	}
	if f.OutputTokensPerReq != 200 {
		t.Fatalf("output per req: %v", f.OutputTokensPerReq)
	}
}

func TestBuildForecastDefaultWindow(t *testing.T) {
	f := BuildForecast([]UpstreamTrafficSample{
		{RequestsPerMinute: 1.0, OutputPerRequest: 100},
	}, 0)
	if f.Window != 15*time.Minute {
		t.Fatalf("zero window should default to 15min: %v", f.Window)
	}
}
