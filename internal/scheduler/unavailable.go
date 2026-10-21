package scheduler

import (
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/upstream"
)

type UnavailableError struct {
	Reason  string
	RetryAt time.Time
}

func (e *UnavailableError) Error() string { return "no upstream available: " + e.Reason }
func (e *UnavailableError) Unwrap() error { return ErrNoUpstream }

func (s *Scheduler) unavailableError(model string, all []*upstream.Upstream, excluded map[int64]bool) error {
	reporter, ok := s.health.(interface {
		Availability(int64, string) health.Availability
	})
	if !ok {
		return ErrNoUpstream
	}
	var selected health.Availability
	for _, candidate := range all {
		if candidate == nil || excluded[candidate.ID] {
			continue
		}
		state := reporter.Availability(candidate.ID, model)
		if state.Reason == "" {
			continue
		}
		if selected.Reason == "" || state.RetryAt.Before(selected.RetryAt) {
			selected = state
		}
	}
	if selected.Reason == "" {
		return ErrNoUpstream
	}
	return &UnavailableError{Reason: selected.Reason, RetryAt: selected.RetryAt}
}
