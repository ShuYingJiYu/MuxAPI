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

// saveCalibrationSample records a successful request with an unadjusted
// candidate window forecast and a usage-derived audit cost.
func saveCalibrationSample(t *testing.T, st *Store, requestID, model string, groupID int64,
	forecast, requests, actual float64, selectedUpstream, actualUpstream int64, at time.Time) {
	t.Helper()
	record := RouteDecisionRecord{
		RequestID: requestID, GroupID: groupID, Model: model, Protocol: "claude",
		Endpoint: "/v1/messages", Reason: "cheapest eligible",
		SelectedUpstreamID: selectedUpstream, ForecastWindow: 15 * time.Minute,
		ForecastRequests: requests, SelectedCost: &forecast, CreatedAt: at,
		Candidates: []RouteCandidateRecord{{
			UpstreamID: selectedUpstream, Selected: true, Eligible: true,
			ForecastTotalCost: &forecast,
		}},
	}
	if _, err := st.SaveRouteDecision(record); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteRouteDecision(requestID, RouteDecisionOutcome{
		ActualCost: &actual, ActualUpstreamID: actualUpstream, Outcome: "success", CompletedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// A window average matching the usage cost should have zero error.
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

// Percent error uses the usage-derived cost as its denominator.
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

// An unset Since excludes old records from the result's time range.
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
	// Keep all other sample fields valid so this exercises only the divisor.
	saveCalibrationSample(t, st, "no-requests", "opus", 1, 0.8, 0, 0.01, 7, 7, now)

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 1 {
		t.Fatalf("samples=%d want only the fully reconciled row", got.Samples)
	}
}

func TestRoutingCalibrationExcludesFailovers(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	saveCalibrationSample(t, st, "same", "opus", 1, 0.8, 80, 0.01, 7, 7, now)
	saveCalibrationSample(t, st, "moved", "opus", 1, 0.8, 80, 100, 7, 9, now)

	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalDecisions != 2 || got.Samples != 1 || got.ExcludedSamples != 1 || got.FailoverSamples != 1 {
		t.Fatalf("unexpected cohort counts: %+v", got)
	}
	if got.ActualCostSum != 0.01 || got.MeanAbsoluteError == nil || *got.MeanAbsoluteError > 1e-9 {
		t.Fatalf("failover cost leaked into comparison: %+v", got)
	}
}

func TestRoutingCalibrationFiltersByGroupAndModel(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	saveCalibrationSample(t, st, "a", "opus", 1, 0.8, 80, 0.01, 7, 7, now)
	saveCalibrationSample(t, st, "b", "sonnet", 2, 8, 4, 1, 7, 7, now)

	byGroup, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour), GroupID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if byGroup.TotalDecisions != 1 || byGroup.Samples != 1 || byGroup.ForecastCostSum != 2 || byGroup.ActualCostSum != 1 {
		t.Fatalf("group filter selected wrong cohort: %+v", byGroup)
	}
	byModel, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour), Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if byModel.TotalDecisions != 1 || byModel.Samples != 1 || byModel.ForecastCostSum != 0.01 || byModel.ActualCostSum != 0.01 {
		t.Fatalf("model filter selected wrong cohort: %+v", byModel)
	}
	intersection, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour), GroupID: 2, Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if intersection.TotalDecisions != 0 || intersection.Samples != 0 {
		t.Fatalf("filters must intersect: %+v", intersection)
	}
}

func TestRoutingCalibrationUsesUnadjustedSelectedCandidate(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	if _, err := st.SaveRouteDecision(RouteDecisionRecord{
		RequestID: "risk-adjusted", SelectedUpstreamID: 7, ForecastRequests: 4,
		SelectedCost: new(8.0), CreatedAt: now,
		Candidates: []RouteCandidateRecord{
			{UpstreamID: 7, Selected: true, Eligible: true, ForecastTotalCost: new(4.0), SuccessRate: 0.5},
			{UpstreamID: 9, Eligible: true, ForecastTotalCost: new(400.0)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteRouteDecision("risk-adjusted", RouteDecisionOutcome{
		ActualCost: new(1.0), ActualUpstreamID: 7, Outcome: "success",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 1 || got.ForecastCostSum != 1 || got.MeanAbsoluteError == nil || *got.MeanAbsoluteError != 0 {
		t.Fatalf("risk penalty or other candidate contaminated comparison: %+v", got)
	}
}

func TestRoutingCalibrationZeroCosts(t *testing.T) {
	tests := []struct {
		name           string
		forecast       float64
		actual         float64
		percentSamples int64
		mape           *float64
		under          float64
		over           float64
	}{
		{name: "zero forecast", forecast: 0, actual: 1, percentSamples: 1, mape: new(1.0), under: 1},
		{name: "zero usage cost", forecast: 1, actual: 0, over: 1},
		{name: "both zero", forecast: 0, actual: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openCalibrationStore(t)
			saveCalibrationSample(t, st, "zero", "opus", 1, tt.forecast, 1, tt.actual, 7, 7, time.Now())
			got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Samples != 1 || got.PercentSamples != tt.percentSamples {
				t.Fatalf("zero cost was dropped or entered wrong denominator: %+v", got)
			}
			if got.MeanAbsoluteError == nil || *got.MeanAbsoluteError != math.Abs(tt.actual-tt.forecast) ||
				got.UnderestimateRate == nil || *got.UnderestimateRate != tt.under ||
				got.OverestimateRate == nil || *got.OverestimateRate != tt.over {
				t.Fatalf("incorrect absolute error or direction: %+v", got)
			}
			if tt.mape == nil {
				if got.MeanAbsolutePercentError != nil {
					t.Fatalf("zero usage cost must leave MAPE undefined: %g", *got.MeanAbsolutePercentError)
				}
			} else if got.MeanAbsolutePercentError == nil || *got.MeanAbsolutePercentError != *tt.mape {
				t.Fatalf("incorrect MAPE: %+v", got)
			}
		})
	}
}

func TestRoutingCalibrationMixedPercentDenominator(t *testing.T) {
	st := openCalibrationStore(t)
	now := time.Now()
	saveCalibrationSample(t, st, "positive", "opus", 1, 2, 1, 1, 7, 7, now)
	saveCalibrationSample(t, st, "zero", "opus", 1, 1, 1, 0, 7, 7, now)
	got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 2 || got.PercentSamples != 1 || got.MeanAbsolutePercentError == nil || *got.MeanAbsolutePercentError != 1 {
		t.Fatalf("zero usage cost diluted MAPE: %+v", got)
	}
}

func TestRoutingCalibrationExcludesIncomparableRecords(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RouteDecisionRecord, *RouteDecisionOutcome)
	}{
		{"missing candidate", func(r *RouteDecisionRecord, _ *RouteDecisionOutcome) { r.Candidates = nil }},
		{"unknown forecast", func(r *RouteDecisionRecord, _ *RouteDecisionOutcome) { r.Candidates[0].ForecastTotalCost = nil }},
		{"unselected candidate", func(r *RouteDecisionRecord, _ *RouteDecisionOutcome) { r.Candidates[0].Selected = false }},
		{"wrong candidate", func(r *RouteDecisionRecord, _ *RouteDecisionOutcome) { r.Candidates[0].UpstreamID = 9 }},
		{"ineligible candidate", func(r *RouteDecisionRecord, _ *RouteDecisionOutcome) { r.Candidates[0].Eligible = false }},
		{"unknown actual upstream", func(_ *RouteDecisionRecord, o *RouteDecisionOutcome) { o.ActualUpstreamID = 0 }},
		{"unknown actual cost", func(_ *RouteDecisionRecord, o *RouteDecisionOutcome) { o.ActualCost = nil }},
		{"negative actual cost", func(_ *RouteDecisionRecord, o *RouteDecisionOutcome) { o.ActualCost = new(-1.0) }},
		{"negative forecast", func(r *RouteDecisionRecord, _ *RouteDecisionOutcome) { r.Candidates[0].ForecastTotalCost = new(-1.0) }},
		{"failed", func(_ *RouteDecisionRecord, o *RouteDecisionOutcome) { o.Outcome = "failed" }},
		{"partial", func(_ *RouteDecisionRecord, o *RouteDecisionOutcome) { o.Outcome = "partial" }},
		{"unknown outcome", func(_ *RouteDecisionRecord, o *RouteDecisionOutcome) { o.Outcome = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openCalibrationStore(t)
			record := RouteDecisionRecord{
				RequestID: "incomparable", SelectedUpstreamID: 7, ForecastRequests: 1,
				SelectedCost: new(1.0), CreatedAt: time.Now(),
				Candidates: []RouteCandidateRecord{{UpstreamID: 7, Selected: true, Eligible: true, ForecastTotalCost: new(1.0)}},
			}
			outcome := RouteDecisionOutcome{ActualCost: new(1.0), ActualUpstreamID: 7, Outcome: "success"}
			tt.mutate(&record, &outcome)
			if _, err := st.SaveRouteDecision(record); err != nil {
				t.Fatal(err)
			}
			if err := st.CompleteRouteDecision(record.RequestID, outcome); err != nil {
				t.Fatal(err)
			}
			got, err := st.RoutingCalibrationSummary(RoutingCalibrationFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if got.TotalDecisions != 1 || got.Samples != 0 || got.ExcludedSamples != 1 ||
				got.ForecastCostSum != 0 || got.ActualCostSum != 0 ||
				got.MeanAbsoluteError != nil || got.MeanAbsolutePercentError != nil {
				t.Fatalf("incomparable record entered the metrics: %+v", got)
			}
		})
	}
}
