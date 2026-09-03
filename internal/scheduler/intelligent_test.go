package scheduler

import (
	"testing"

	"github.com/mirainya/muxapi/internal/routing"
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
