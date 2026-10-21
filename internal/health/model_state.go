package health

import "time"

func transientCooldown(failures int) time.Duration {
	switch failures {
	case 0, 1:
		return 5 * time.Second
	case 2:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

// Synchronize the latest state, rather than a snapshot captured before waiting
// for storage. A delayed failure can therefore never recreate a recovered row.
func (m *Manager) syncModel(key modelKey) error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	return m.syncModelLocked(key)
}

func (m *Manager) syncModelLocked(key modelKey) error {
	m.mu.Lock()
	exclusion := m.unsupported[key]
	copy := exclusion.publicCopy()
	persist, remove := m.persistModel, m.deleteModel
	m.mu.Unlock()
	if exclusion != nil && persist != nil {
		return persist(copy)
	}
	if exclusion == nil && remove != nil {
		return remove(key.upstreamID, key.model)
	}
	return nil
}

// Reconfigure invalidates health evidence belonging to the previous endpoint
// or credentials. Statistics remain intact, while in-flight results are fenced
// by the new generation. Storage errors leave the old in-memory state intact.
func (m *Manager) Reconfigure(id int64, save func() error) error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	var keys []modelKey
	for key := range m.unsupported {
		if key.upstreamID == id {
			keys = append(keys, key)
		}
	}
	remove := m.deleteModel
	m.mu.Unlock()
	// The configuration save clears durable exclusions in the same transaction
	// as the endpoint/credential update. Serialize it with pending health writes.
	if save != nil {
		if err := save(); err != nil {
			return err
		}
	} else if remove != nil {
		for _, key := range keys {
			if err := remove(id, key.model); err != nil {
				return err
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(id)
	b.generation++
	b.state, b.fails, b.reopenCount = Closed, 0, 0
	b.recoveryLease, b.recoverySuccesses = 0, 0
	b.openUntil, b.trialNotBefore, b.failureWindowFrom = time.Time{}, time.Time{}, time.Time{}
	b.lastSuccess, b.lastTransportFail = time.Time{}, time.Time{}
	b.latencyMs, b.latencyEWMA, b.earlyTrial = 0, 0, false
	for key := range m.unsupported {
		if key.upstreamID == id {
			delete(m.unsupported, key)
		}
	}
	for key := range m.rateLimits {
		if key.upstreamID == id {
			delete(m.rateLimits, key)
		}
	}
	for key := range m.capLatest {
		if key.upstreamID == id {
			delete(m.capLatest, key)
		}
	}
	return nil
}

// Availability describes local rejection without changing recovery ownership.
type Availability struct {
	Reason  string
	Message string
	RetryAt time.Time
}

func (m *Manager) Availability(id int64, model string) Availability {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := modelKey{id, model}
	busy := Availability{Reason: "recovery_in_progress", Message: "an upstream recovery request is in progress", RetryAt: time.Now().Add(time.Second)}
	if exclusion := m.unsupported[key]; exclusion != nil {
		if exclusion.reprobeLease != 0 {
			return busy
		}
		if exclusion.ExcludedUntil != nil && time.Now().Before(*exclusion.ExcludedUntil) {
			return Availability{Reason: "model_unsupported", Message: exclusion.LastReason, RetryAt: *exclusion.ExcludedUntil}
		}
	}
	if limit := m.rateLimits[key]; limit != nil {
		if limit.recoveryLease != 0 {
			return busy
		}
		if time.Now().Before(limit.until) {
			reason := "upstream_temporary_failure"
			if limit.kind == "rate_limit" {
				reason = "upstream_rate_limit"
			}
			return Availability{Reason: reason, Message: limit.lastReason, RetryAt: limit.until}
		}
	}
	b := m.get(id)
	if b.recoveryLease != 0 {
		return busy
	}
	if b.state == Open {
		retryAt := b.openUntil
		if b.trialNotBefore.Before(retryAt) {
			retryAt = b.trialNotBefore
		}
		return Availability{Reason: "upstream_circuit_open", Message: "upstream recovery is cooling down", RetryAt: retryAt}
	}
	return Availability{}
}
