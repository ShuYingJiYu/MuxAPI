package store

import "time"

// defaultCalibrationWindow bounds a calibration query that arrives without an
// explicit start, so an unset filter can never turn into a full-table scan.
const defaultCalibrationWindow = 24 * time.Hour

// RoutingCalibrationFilter selects the completed routing decisions used to
// compare the per-request forecast cost with the authoritative cost observed
// after forwarding.
type RoutingCalibrationFilter struct {
	Since   time.Time
	GroupID int64
	Model   string
}

// RoutingCalibration is deliberately aggregate-only. Detailed request and
// upstream dimensions remain in route_decisions and are not exported as
// Prometheus labels.
//
// Ratio fields are nil when there is no sample to divide by, so a caller can
// tell "no data" apart from "a perfect forecast". A zero value would read as
// zero error.
type RoutingCalibration struct {
	Since                    int64    `json:"since"`
	Samples                  int64    `json:"samples"`
	PercentSamples           int64    `json:"percent_samples"`
	ForecastCostSum          float64  `json:"forecast_cost_sum"`
	ActualCostSum            float64  `json:"actual_cost_sum"`
	AbsoluteErrorSum         float64  `json:"absolute_error_sum"`
	FailoverSamples          int64    `json:"failover_samples"`
	MeanAbsoluteError        *float64 `json:"mean_absolute_error"`
	MeanAbsolutePercentError *float64 `json:"mean_absolute_percent_error"`
	UnderestimateRate        *float64 `json:"underestimate_rate"`
	OverestimateRate         *float64 `json:"overestimate_rate"`
}

// RoutingCalibrationSummary returns a bounded, read-only calibration summary.
//
// selected_cost is a forecast total for the whole decision window (cost.go
// multiplies every term by forecast.Requests), while actual_cost is the price
// of the single request that was actually forwarded. Comparing them directly
// makes the forecast look ~77x too large on this fleet, so the forecast is
// divided by forecast_requests before any error is computed.
//
// A sample is included only when both costs are known and positive and the
// forecast window covers at least one request; missing or zero prices are not
// an error signal.
func (s *Store) RoutingCalibrationSummary(filter RoutingCalibrationFilter) (*RoutingCalibration, error) {
	since := filter.Since
	if since.IsZero() {
		since = time.Now().Add(-defaultCalibrationWindow)
	}
	// CAST keeps SQLite and PostgreSQL on the same floating-point division.
	const perRequest = "(CAST(selected_cost AS REAL)/CAST(forecast_requests AS REAL))"
	where := " WHERE created_at>=? AND selected_cost IS NOT NULL AND actual_cost IS NOT NULL" +
		" AND selected_cost>0 AND forecast_requests>0"
	args := []any{s.timeValue(since)}
	if filter.GroupID > 0 {
		where += " AND group_id=?"
		args = append(args, filter.GroupID)
	}
	if filter.Model != "" {
		where += " AND model=?"
		args = append(args, filter.Model)
	}
	result := &RoutingCalibration{Since: since.Unix()}
	var underestimate, overestimate int64
	var percentErrorSum float64
	row := s.queryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(`+perRequest+`),0),
		COALESCE(SUM(actual_cost),0),
		COALESCE(SUM(ABS(actual_cost-(`+perRequest+`))),0),
		COALESCE(SUM(CASE WHEN actual_cost>(`+perRequest+`) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN actual_cost<(`+perRequest+`) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN actual_upstream_id>0 AND selected_upstream_id>0
			AND actual_upstream_id<>selected_upstream_id THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN actual_cost>0 THEN ABS(actual_cost-(`+perRequest+`))/actual_cost ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN actual_cost>0 THEN 1 ELSE 0 END),0)
		FROM route_decisions`+where, args...)
	if err := row.Scan(&result.Samples, &result.ForecastCostSum, &result.ActualCostSum,
		&result.AbsoluteErrorSum, &underestimate, &overestimate, &result.FailoverSamples,
		&percentErrorSum, &result.PercentSamples); err != nil {
		return nil, err
	}
	if result.Samples > 0 {
		result.MeanAbsoluteError = new(result.AbsoluteErrorSum / float64(result.Samples))
		result.UnderestimateRate = new(float64(underestimate) / float64(result.Samples))
		result.OverestimateRate = new(float64(overestimate) / float64(result.Samples))
	}
	// MAPE divides by the observed cost, not the forecast: "how wrong were we"
	// is relative to the truth. Samples with a zero actual cost are excluded
	// from the ratio instead of being counted as zero error.
	if result.PercentSamples > 0 {
		result.MeanAbsolutePercentError = new(percentErrorSum / float64(result.PercentSamples))
	}
	return result, nil
}
