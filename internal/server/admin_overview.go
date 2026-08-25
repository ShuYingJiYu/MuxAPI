package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

type adminOverviewSummaryResponse struct {
	TodayFromAt   int64                      `json:"today_from_at"`
	WeekFromAt    int64                      `json:"week_from_at"`
	ToAt          int64                      `json:"to_at"`
	TodayRequests int64                      `json:"today_requests"`
	WeekCost      store.OverviewCostEstimate `json:"week_cost"`
}

func overviewPeriodStarts(now time.Time) (time.Time, time.Time) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	daysSinceMonday := (int(today.Weekday()) + 6) % 7
	return today, today.AddDate(0, 0, -daysSinceMonday)
}

func (s *Server) adminOverviewSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now()
	today, week := overviewPeriodStarts(now)
	stats, err := s.store.RequestStats(store.RequestFilter{Since: today})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cost, err := s.store.OverviewUsageCost(week.Unix(), now.Unix())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, adminOverviewSummaryResponse{
		TodayFromAt: today.Unix(), WeekFromAt: week.Unix(), ToAt: now.Unix(),
		TodayRequests: stats.Total, WeekCost: cost,
	})
}

// adminOverviewTrends returns chart-ready totals for the dashboard scope.
// tag_id selects enabled upstreams whose primary tag matches; group_id remains supported for compatibility.
func (s *Server) adminOverviewTrends(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	groupText := strings.TrimSpace(r.URL.Query().Get("group_id"))
	tagText := strings.TrimSpace(r.URL.Query().Get("tag_id"))
	var groupID int64
	var tagID int64
	var err error
	if groupText != "" {
		groupID, err = strconv.ParseInt(groupText, 10, 64)
		if err != nil || groupID <= 0 {
			http.Error(w, "invalid group_id", http.StatusBadRequest)
			return
		}
	}
	if tagText != "" {
		tagID, err = strconv.ParseInt(tagText, 10, 64)
		if err != nil || tagID <= 0 {
			http.Error(w, "invalid tag_id", http.StatusBadRequest)
			return
		}
	}
	if groupID > 0 && tagID > 0 {
		http.Error(w, "group_id and tag_id are mutually exclusive", http.StatusBadRequest)
		return
	}
	window := store.LookupOverviewTrendWindow(r.URL.Query().Get("window"))
	var trend *store.OverviewTrends
	if tagID > 0 {
		trend, err = s.store.OverviewTrendsByTag(tagID, window, time.Now())
	} else {
		trend, err = s.store.OverviewTrends(groupID, window, time.Now())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, trend)
}
