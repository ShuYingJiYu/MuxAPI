package forward

import (
	"testing"
	"time"
)

func TestComputeAdaptiveTimeout(t *testing.T) {
	tests := []struct {
		name   string
		params AdaptiveTimeoutParams
		want   time.Duration
	}{
		{
			name: "cold start uses ceiling",
			params: AdaptiveTimeoutParams{
				P95Ms: 3000, Samples: 3, InputTokens: 100000,
				Multiplier: 2.0, Floor: 10 * time.Second, Ceiling: 120 * time.Second,
			},
			want: 120 * time.Second,
		},
		{
			name: "normal small request",
			params: AdaptiveTimeoutParams{
				P95Ms: 3200, Samples: 50, InputTokens: 4000,
				Multiplier: 2.0, Floor: 10 * time.Second, Ceiling: 120 * time.Second,
			},
			want: 10 * time.Second, // 3200*2=6400ms < floor
		},
		{
			name: "large context adds token bonus",
			params: AdaptiveTimeoutParams{
				P95Ms: 3200, Samples: 50, InputTokens: 140000,
				Multiplier: 2.0, Floor: 10 * time.Second, Ceiling: 120 * time.Second,
			},
			// 3200*2=6400ms + 140000/50000*5s = 6.4s + 10s = 16.4s
			want: 16400 * time.Millisecond,
		},
		{
			name: "slow upstream large context",
			params: AdaptiveTimeoutParams{
				P95Ms: 25000, Samples: 100, InputTokens: 140000,
				Multiplier: 2.0, Floor: 10 * time.Second, Ceiling: 120 * time.Second,
			},
			// 25000*2=50s + 140000/50000*5s = 50s + 10s = 60s
			want: 60 * time.Second,
		},
		{
			name: "capped at ceiling",
			params: AdaptiveTimeoutParams{
				P95Ms: 80000, Samples: 20, InputTokens: 200000,
				Multiplier: 2.0, Floor: 10 * time.Second, Ceiling: 120 * time.Second,
			},
			// 80000*2=160s + 20s = 180s > ceiling
			want: 120 * time.Second,
		},
		{
			name: "zero multiplier defaults to 2",
			params: AdaptiveTimeoutParams{
				P95Ms: 5000, Samples: 10, InputTokens: 50000,
				Floor: 10 * time.Second, Ceiling: 120 * time.Second,
			},
			// 5000*2=10s + 50000/50000*5s = 10s + 5s = 15s
			want: 15 * time.Second,
		},
		{
			name: "explicit zero token bonus disables context extension",
			params: AdaptiveTimeoutParams{
				P95Ms: 5000, Samples: 10, InputTokens: 50000,
				Floor: 10 * time.Second, Ceiling: 120 * time.Second,
				TokenBonusConfigured: true,
			},
			want: 10 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeAdaptiveTimeout(tt.params)
			if got != tt.want {
				t.Errorf("ComputeAdaptiveTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}
