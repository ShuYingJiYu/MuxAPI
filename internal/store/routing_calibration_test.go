package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func openCalibrationStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "calibration.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// saveCalibrationSample stores one completed decision. forecast is the total
// for the whole window (as cost.go computes it), actual is the single request.
func saveCalibrationSample(t *testing.T, st *Store, requestID, model string, groupID int64,
	forecast, requests, actual float64, selectedUpstream, actualUpstream int64, at time.Time) {
	t.Helper()
	record := RouteDecisionRecord{
		RequestID: requestID, GroupID: groupID, Model: model, Protocol: "claude",
		Endpoint: "/v1/messages", Reason: "cheapest eligible",
		SelectedUpstreamID: selectedUpstream, ForecastWindow: 15 * time.Minute,
		ForecastRequests: requests, SelectedCost: &forecast, CreatedAt: at,
	}
	if _, err := st.SaveRouteDecision(record); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteRouteDecision(requestID, RouteDecisionOutcome{
		ActualCost: &actual, ActualUpstreamID: actualUpstream, CompletedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// selected_cost is a window total, actual_cost is one request. A forecast that
// is correct per request must not be reported as a 80x overestimate.
func TestRoutingCalibrationNormalizesWindowForecast(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	// 80 requests forecast at 0.01 each => 0.8 total, one request actually 0.01.
	saveCalibrationSample(t, st, "req-1", "opus", 1, 0.8, 80, 0.01, 7, 7, now)

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 1 {
		t.Fatalf("samples=%d want 1", got.Samples)
	}
	if math.Abs(got.ForecastCostSum-0.01) > 1e-9 {
		t.Fatalf("forecast sum=%.9f want the per-request 0.01", got.ForecastCostSum)
	}
	if got.MeanAbsoluteError == nil || *got.MeanAbsoluteError > 1e-9 {
		t.Fatalf("a correct per-request forecast must not report error: %+v", got.MeanAbsoluteError)
	}
	if got.OverestimateRate == nil || *got.OverestimateRate != 0 {
		t.Fatalf("overestimate rate=%v want 0", got.OverestimateRate)
	}
}

// MAPE must divide by the observed cost: "how wrong were we" is relative to the
// truth, not to our own estimate.
func TestRoutingCalibrationPercentErrorUsesActualCost(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	// Forecast 0.02 per request (2.0 over 100 requests), actual 0.01:
	// |error|/actual = 0.01/0.01 = 1.0, while dividing by our own forecast
	// would report a misleading 0.5.
	saveCalibrationSample(t, st, "req-1", "opus", 1, 2.0, 100, 0.01, 7, 7, now)

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.MeanAbsolutePercentError == nil || math.Abs(*got.MeanAbsolutePercentError-1.0) > 1e-9 {
		t.Fatalf("MAPE=%v want 1.0 (relative to actual cost)", got.MeanAbsolutePercentError)
	}
	if got.OverestimateRate == nil || *got.OverestimateRate != 1 {
		t.Fatalf("overestimate rate=%v want 1", got.OverestimateRate)
	}
}

// No samples must be distinguishable from a perfect forecast.
func TestRoutingCalibrationEmptyWindowReturnsNilRatios(t *testing.T) {
	st := openCalibrationStore(t)

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 0 {
		t.Fatalf("samples=%d want 0", got.Samples)
	}
	if got.MeanAbsoluteError != nil || got.MeanAbsolutePercentError != nil ||
		got.UnderestimateRate != nil || got.OverestimateRate != nil {
		t.Fatalf("empty window must report nil ratios, got %+v", got)
	}
}

// An unset Since must fall back to a bounded window instead of scanning the
// whole table.
func TestRoutingCalibrationDefaultsToBoundedWindow(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	saveCalibrationSample(t, st, "recent", "opus", 1, 0.8, 80, 0.01, 7, 7, now.Add(-time.Hour))
	saveCalibrationSample(t, st, "stale", "opus", 1, 0.8, 80, 0.01, 7, 7, now.Add(-30*24*time.Hour))

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 1 {
		t.Fatalf("samples=%d want 1: a zero Since must not include month-old rows", got.Samples)
	}
	if since := time.Unix(got.Since, 0); since.Before(now.Add(-25 * time.Hour)) {
		t.Fatalf("since=%v want a bounded default window", since)
	}
}

// Rows without a usable forecast must not silently dilute the ratios.
func TestRoutingCalibrationSkipsUnknownCosts(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	saveCalibrationSample(t, st, "good", "opus", 1, 0.8, 80, 0.01, 7, 7, now)
	// No CompleteRouteDecision call: actual_cost stays NULL.
	forecast := 0.8
	if _, err := st.SaveRouteDecision(RouteDecisionRecord{
		RequestID: "no-actual", GroupID: 1, Model: "opus", Protocol: "claude",
		Endpoint: "/v1/messages", SelectedUpstreamID: 7, ForecastWindow: 15 * time.Minute,
		ForecastRequests: 80, SelectedCost: &forecast, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Zero forecast requests would divide by zero.
	if _, err := st.SaveRouteDecision(RouteDecisionRecord{
		RequestID: "no-requests", GroupID: 1, Model: "opus", Protocol: "claude",
		Endpoint: "/v1/messages", SelectedUpstreamID: 7, ForecastWindow: 15 * time.Minute,
		ForecastRequests: 0, SelectedCost: &forecast, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 1 {
		t.Fatalf("samples=%d want only the fully reconciled row", got.Samples)
	}
}

func TestRoutingCalibrationCountsFailovers(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	saveCalibrationSample(t, st, "same", "opus", 1, 0.8, 80, 0.01, 7, 7, now)
	saveCalibrationSample(t, st, "moved", "opus", 1, 0.8, 80, 0.01, 7, 9, now)

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.FailoverSamples != 1 {
		t.Fatalf("failover samples=%d want 1", got.FailoverSamples)
	}
}

func TestRoutingCalibrationFiltersByGroupAndModel(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	saveCalibrationSample(t, st, "a", "opus", 1, 0.8, 80, 0.01, 7, 7, now)
	saveCalibrationSample(t, st, "b", "sonnet", 2, 0.8, 80, 0.01, 7, 7, now)

	byGroup, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour), GroupID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if byGroup.Samples != 1 {
		t.Fatalf("group filter samples=%d want 1", byGroup.Samples)
	}
	byModel, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour), Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if byModel.Samples != 1 {
		t.Fatalf("model filter samples=%d want 1", byModel.Samples)
	}
}
