package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestBlockHardLimitRejectionsPreservesOrdinaryFallbacks(t *testing.T) {
	blocked := map[int64]bool{}
	blockHardLimitRejections(blocked, []routing.CandidateEvaluation{
		{CandidateID: 1, Samples: 20, P95TTFTMs: 1500},
		{CandidateID: 2, Samples: 20, RejectReason: "pricing is incomplete"},
		{CandidateID: 3, Samples: 20, P95DurationMs: 5000},
		{CandidateID: 4, Samples: 0, P95TTFTMs: 5000},
	}, routing.Config{MaxTTFTMs: 1000, MaxDurationMs: 4000})

	if !blocked[1] || !blocked[3] {
		t.Fatalf("hard-limit candidates were not blocked: %+v", blocked)
	}
	if blocked[2] || blocked[4] {
		t.Fatalf("unknown price or cold history must remain eligible for P2C fallback: %+v", blocked)
	}
}

func TestModelReturnsCache(t *testing.T) {
	queryErr := errors.New("db")
	cases := []struct {
		name     string
		evidence store.ModelCacheEvidence
		err      error
		want     bool
	}{
		{name: "query error keeps cache pricing", err: queryErr, want: true},
		{name: "too few samples keeps the prior", evidence: store.ModelCacheEvidence{Successes: 19}, want: true},
		{name: "enough cache-free successes disables cache pricing", evidence: store.ModelCacheEvidence{Successes: 20}, want: false},
		{name: "any cache use keeps cache pricing", evidence: store.ModelCacheEvidence{Successes: 100, CacheUses: 1}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelReturnsCache(tc.evidence, tc.err); got != tc.want {
				t.Fatalf("modelReturnsCache = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCoverageBucketStaysInsideQueryBand(t *testing.T) {
	for prefix := int64(1); prefix <= 300_000; prefix++ {
		b := coverageBucket(prefix)
		width := b / 4
		if width < 1 {
			width = 1
		}
		if prefix < b || prefix >= b+width {
			t.Fatalf("prefix %d bucket %d is wider than 1.25x", prefix, b)
		}
	}
	if coverageBucket(600) == coverageBucket(2559) {
		t.Fatalf("short prefix shares the 2048 bucket: %d", coverageBucket(600))
	}
}

func TestFallbackDecisionKeepsEvaluationsAndCost(t *testing.T) {
	last := routing.Decision{
		SelectedID: 9, SelectedName: "unclaimed",
		Reason: "cost",
		Evaluations: []routing.CandidateEvaluation{
			{CandidateID: 1, CandidateName: "rejected", RejectReason: "OPEN circuit", Cost: routing.CostEstimate{SelectedTotal: 3.5}},
			{CandidateID: 7, CandidateName: "p2c", Cost: routing.CostEstimate{SelectedTotal: 1.25}, EffectiveCost: 1.1},
		},
		Forecast: routing.TrafficForecast{Window: time.Minute, Requests: 4},
	}
	got := fallbackDecision(last, &upstream.Upstream{ID: 7, Name: "p2c"}, time.Hour)

	if got.SelectedID != 7 || got.SelectedName != "p2c" {
		t.Fatalf("fallback must select the P2C candidate: %+v", got)
	}
	if len(got.Evaluations) != len(last.Evaluations) {
		t.Fatalf("fallback dropped the rejected candidates: %+v", got.Evaluations)
	}
	if got.EffectiveCost != 1.1 || got.Cost.SelectedTotal != 1.25 {
		t.Fatalf("fallback must carry the selected candidate's cost: %+v", got)
	}
	if got.Forecast.Window != time.Minute || got.Forecast.Requests != 4 {
		t.Fatalf("fallback must keep the computed forecast: %+v", got.Forecast)
	}
}

func TestFallbackDecisionWithoutPriorEvaluationStaysEmpty(t *testing.T) {
	got := fallbackDecision(routing.Decision{}, &upstream.Upstream{ID: 3, Name: "p2c"}, time.Hour)
	if got.SelectedID != 3 || got.Reason == "" {
		t.Fatalf("fallback must still choose the candidate: %+v", got)
	}
	if len(got.Evaluations) != 0 || got.Forecast.Window != time.Hour {
		t.Fatalf("no prior evaluation must yield no candidates and a window-only forecast: %+v", got)
	}
}
