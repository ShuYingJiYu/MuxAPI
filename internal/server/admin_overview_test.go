package server

import (
	"testing"
	"time"
)

func TestOverviewPeriodStartsOnMonday(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, time.August, 25, 13, 45, 0, 0, location)
	today, week := overviewPeriodStarts(now)
	wantToday := time.Date(2026, time.August, 25, 0, 0, 0, 0, location)
	wantWeek := time.Date(2026, time.August, 24, 0, 0, 0, 0, location)
	if !today.Equal(wantToday) || !week.Equal(wantWeek) {
		t.Fatalf("unexpected period starts: today=%s week=%s", today, week)
	}
}
