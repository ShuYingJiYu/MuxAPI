package health

import (
	"testing"
	"time"
)

type delayedExclusionStore struct {
	*memoryExclusionStore
	started chan struct{}
	resume  chan struct{}
	deleted chan struct{}
}

func (s *delayedExclusionStore) UpsertModelExclusion(exclusion ModelExclusion) error {
	close(s.started)
	<-s.resume
	return s.memoryExclusionStore.UpsertModelExclusion(exclusion)
}

func (s *delayedExclusionStore) DeleteModelExclusion(id int64, model string) error {
	select {
	case s.deleted <- struct{}{}:
	default:
	}
	return s.memoryExclusionStore.DeleteModelExclusion(id, model)
}

func TestRecoverySurvivesDelayedPersistentFailureAcrossRestart(t *testing.T) {
	storage := &delayedExclusionStore{memoryExclusionStore: newMemoryExclusionStore(), started: make(chan struct{}), resume: make(chan struct{}), deleted: make(chan struct{}, 1)}
	m := New(3, time.Minute)
	if err := m.SetModelExclusionStore(storage); err != nil {
		t.Fatal(err)
	}
	lease, _ := m.Claim(1, 7, "model")
	finished := make(chan struct{})
	go func() { m.CompleteModelUnsupported(lease, 0, 404, "old failure"); close(finished) }()
	<-storage.started
	recovered := make(chan error, 1)
	go func() { recovered <- m.RecoverModel(7, "model") }()
	// In the broken implementation recovery deletes the row while the old
	// upsert is paused. The fixed implementation serializes these operations.
	select {
	case <-storage.deleted:
	case <-time.After(20 * time.Millisecond):
	}
	close(storage.resume)
	<-finished
	if err := <-recovered; err != nil {
		t.Fatal(err)
	}
	restarted := New(3, time.Minute)
	if err := restarted.SetModelExclusionStore(storage); err != nil {
		t.Fatal(err)
	}
	if !restarted.IsAvailable(7, "model") {
		t.Fatal("delayed old write recreated the recovered exclusion after restart")
	}
}
