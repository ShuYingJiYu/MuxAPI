package health

import (
	"sync"
	"testing"
	"time"
)

func TestTransientFailureUsesIndependentBackoffAndSingleRecovery(t *testing.T) {
	m := New(3, 10*time.Minute)
	for _, expected := range []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second} {
		lease, ok := m.Claim(1, 7, "model")
		if !ok {
			t.Fatal("recovery claim failed")
		}
		m.CompleteModelTransientFailure(lease, 5, 200, "temporary failure")
		limit := m.rateLimits[modelKey{7, "model"}]
		if got := limit.until.Sub(limit.failedAt); got != expected {
			t.Fatalf("cooldown = %s, want %s", got, expected)
		}
		limit.until = time.Now().Add(-time.Second)
	}
	const concurrency = 20
	var workers sync.WaitGroup
	claims := make(chan Lease, concurrency)
	start := make(chan struct{})
	for index := 0; index < concurrency; index++ {
		workers.Add(1)
		go func(group int64) {
			defer workers.Done()
			<-start
			if lease, ok := m.Claim(group, 7, "model"); ok {
				claims <- lease
			}
		}(int64(index + 1))
	}
	close(start)
	workers.Wait()
	close(claims)
	if len(claims) != 1 {
		t.Fatalf("expired cooldown admitted %d concurrent recovery requests", len(claims))
	}
	for lease := range claims {
		m.Release(lease)
	}
	lease, ok := m.Claim(2, 7, "model")
	if !ok {
		t.Fatal("cancel did not release recovery ownership")
	}
	m.Complete(lease, ResultSuccess, 5)
	if len(m.ModelStates(7)) != 0 || !m.IsAvailable(7, "model") {
		t.Fatal("successful recovery did not clear model cooldown")
	}
}

func TestLastResortDistinguishesTransientFailuresAndRateLimits(t *testing.T) {
	m := New(3, time.Minute)
	lease, _ := m.Claim(1, 7, "transient")
	m.CompleteModelTransientFailure(lease, 5, 200, "overloaded")
	trial, ok := m.ClaimLastResort(2, 7, "transient")
	if !ok {
		t.Fatal("all-unavailable path must admit one transient trial")
	}
	if _, ok := m.ClaimLastResort(3, 7, "transient"); ok {
		t.Fatal("recovery must be exclusive across groups")
	}
	m.CompleteModelTransientFailure(trial, 5, 200, "still overloaded")
	if _, ok := m.ClaimLastResort(2, 7, "transient"); ok {
		t.Fatal("failed trial must wait for backoff")
	}
	lease, _ = m.Claim(1, 7, "limited")
	m.CompleteModelRateLimited(lease, 5, time.Minute, 429, "rate limited")
	if _, ok := m.ClaimLastResort(2, 7, "limited"); ok {
		t.Fatal("last resort bypassed Retry-After")
	}
	if probe := m.BeginProbe(7, "limited"); probe.Valid() {
		t.Fatal("monitoring bypassed Retry-After")
	}
}

func TestLegacyPermanentExclusionExpiresAndReconfigurationFencesOldResults(t *testing.T) {
	storage := newMemoryExclusionStore()
	key := modelKey{7, "model"}
	storage.records[key] = ModelExclusion{UpstreamID: 7, Model: "model", LastFailedAt: time.Now().Add(-time.Hour)}
	m := New(3, time.Minute)
	m.SetAdvancedPolicy(2, time.Minute, 0)
	if err := m.SetModelExclusionStore(storage); err != nil {
		t.Fatal(err)
	}
	lease, ok := m.Claim(1, 7, "model")
	if !ok {
		t.Fatal("legacy permanent exclusion was not made eligible for re-probe")
	}
	if storage.records[key].ExcludedUntil == nil {
		t.Fatal("legacy exclusion was not normalized in storage")
	}
	if err := m.Reconfigure(7, nil); err != nil {
		t.Fatal(err)
	}
	m.CompleteModelUnsupported(lease, 0, 404, "late result from old credentials")
	if !m.IsAvailable(7, "model") || len(storage.records) != 0 || m.InFlight(7) != 0 {
		t.Fatal("old result recreated exclusion after configuration change")
	}
}

func TestFailedModelRecoveryTransportAttemptBacksOff(t *testing.T) {
	m := New(3, 10*time.Minute)
	lease, _ := m.Claim(1, 7, "model")
	m.CompleteModelTransientFailure(lease, 5, 200, "overloaded")
	trial, ok := m.ClaimLastResort(2, 7, "model")
	if !ok {
		t.Fatal("recovery claim failed")
	}
	m.CompleteTransportFailure(trial, 0)
	if _, ok := m.ClaimLastResort(3, 7, "model"); ok {
		t.Fatal("failed transport recovery bypassed its backoff")
	}
	if m.InFlight(7) != 0 {
		t.Fatal("failed transport recovery leaked occupancy")
	}
}

func TestModelDiscoveryDoesNotSupersedeAnActiveRecoveryRequest(t *testing.T) {
	m := New(3, time.Minute)
	lease, _ := m.Claim(1, 7, "model")
	m.CompleteModelTransientFailure(lease, 5, 200, "overloaded")
	trial, ok := m.ClaimLastResort(2, 7, "model")
	if !ok {
		t.Fatal("recovery claim failed")
	}
	m.MarkModelsDiscovered(7, []string{"model"})
	m.Complete(trial, ResultSuccess, 5)
	if len(m.ModelStates(7)) != 0 || !m.IsAvailable(7, "model") {
		t.Fatal("catalog discovery prevented a real successful recovery")
	}
}
