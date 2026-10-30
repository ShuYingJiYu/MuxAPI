package server

import (
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/store"
)

// A fallback row written through persistRoutingAudit must store selected_cost as
// NULL (unknown) rather than 0 (free), and must be labelled p2c-fallback.
func TestPersistRoutingAuditFallbackStoresUnknownCost(t *testing.T) {
	_, st, _ := newAdminTestServer(t)

	decision := routing.Decision{
		SelectedID: 7, SelectedName: "p2c",
		Reason:   "fallback: cost data incomplete; standard health/P2C scheduler",
		Forecast: routing.TrafficForecast{Window: time.Minute},
		// No Evaluations: the fallback candidate had no resolved pricing.
	}
	srv := &Server{store: st}
	srv.persistRoutingAudit("req-fallback", time.Now(), 1, "m", "/v1/messages",
		forward.Result{RouteDecision: &decision, FinalUpstreamID: 7})

	rows, err := st.ListRouteDecisions(store.RouteDecisionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 decision, got %d", len(rows))
	}
	got := rows[0]
	if got.Strategy != "p2c-fallback" {
		t.Fatalf("strategy=%q want p2c-fallback", got.Strategy)
	}
	if got.SelectedCost != nil {
		t.Fatalf("selected_cost=%v want NULL for a fallback without pricing", *got.SelectedCost)
	}
}