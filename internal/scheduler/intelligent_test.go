package scheduler

import (
	"errors"
	"testing"

	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
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
