package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

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
