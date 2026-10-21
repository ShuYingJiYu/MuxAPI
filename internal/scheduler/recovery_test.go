package scheduler

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/upstream"
)

type claimRaceHealth struct {
	*health.Manager
	failed atomic.Bool
}

func (h *claimRaceHealth) Claim(groupID, id int64, model string) (health.Lease, bool) {
	if h.failed.CompareAndSwap(false, true) {
		return health.Lease{}, false
	}
	return h.Manager.Claim(groupID, id, model)
}

func TestTransientClaimRaceDoesNotPermanentlyExcludeRecoveredCandidate(t *testing.T) {
	manager := &claimRaceHealth{Manager: health.New(3, time.Minute)}
	channels := []*upstream.Upstream{{ID: 7, Priority: 1}}
	sched := New(func(int64) []*upstream.Upstream { return channels }, manager)
	chosen, lease, err := sched.Pick(1, "model")
	if err != nil || chosen == nil || chosen.ID != 7 {
		t.Fatalf("temporary claim conflict prevented recovery selection: %v", err)
	}
	manager.Release(lease)
	if _, _, err := sched.PickExcluding(1, "model", map[int64]bool{7: true}); err == nil {
		t.Fatal("an already-attempted candidate was retried")
	}
}

func TestHealthyBackupIsPreferredOverTransientRecovery(t *testing.T) {
	manager := health.New(3, 10*time.Minute)
	lease, _ := manager.Claim(1, 1, "model")
	manager.CompleteModelTransientFailure(lease, 5, 200, "overloaded")
	channels := []*upstream.Upstream{{ID: 1, Priority: 1}, {ID: 2, Priority: 2}}
	sched := New(func(int64) []*upstream.Upstream { return channels }, manager)
	chosen, lease, err := sched.Pick(1, "model")
	if err != nil || chosen.ID != 2 {
		t.Fatalf("transient recovery was preferred over a healthy channel: %v", err)
	}
	manager.Release(lease)
}
