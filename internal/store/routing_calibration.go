package store

import "time"

// defaultCalibrationWindow limits the time range when the caller omits Since.
const defaultCalibrationWindow = 24 * time.Hour

// RoutingCalibrationFilter selects routing decisions for an audit comparison.
type RoutingCalibrationFilter struct {
	Since   time.Time
	GroupID int64
	Model   string
}

// RoutingCalibration compares unadjusted candidate window averages with costs
// calculated from usage and the recorded pricing snapshot. These are not
// predictions for the current request or independently verified charges.
// Samples counts comparable successful decisions; all other filtered decisions
// remain visible through TotalDecisions and ExcludedSamples. FailoverSamples is
// a subset of ExcludedSamples. Ratio fields are nil without a denominator.
type RoutingCalibration struct {
	Since                    int64    `json:"since"`
	ComparisonBasis          string   `json:"comparison_basis"`
	TotalDecisions           int64    `json:"total_decisions"`
	ExcludedSamples          int64    `json:"excluded_samples"`
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

// RoutingCalibrationSummary is read-only. It uses the selected candidate's
// forecast_total_cost, not selected_cost, which may include a success-rate
// penalty. Only successful requests served by that same upstream are comparable.
// Dividing a window forecast by its request count gives a window average; cache
// creation and TTL amortization mean it need not predict the current request.
func (s *Store) RoutingCalibrationSummary(filter RoutingCalibrationFilter) (*RoutingCalibration, error) {
	since := filter.Since
	if since.IsZero() {
		since = time.Now().Add(-defaultCalibrationWindow)
	}
	where := " WHERE rd.created_at>=?"
	args := []any{s.timeValue(since)}
	if filter.GroupID > 0 {
		where += " AND rd.group_id=?"
		args = append(args, filter.GroupID)
	}
	if filter.Model != "" {
		where += " AND rd.model=?"
		args = append(args, filter.Model)
	}
	result := &RoutingCalibration{
		Since:           since.Unix(),
		ComparisonBasis: "selected_candidate_window_average_vs_usage_cost",
	}
	var underestimate, overestimate int64
	var percentErrorSum float64
	// DOUBLE PRECISION retains float64 precision on PostgreSQL (REAL is float32)
	// and gives floating-point division on SQLite. NULLIF also guards the divisor
	// independently of the eligibility checks.
	row := s.queryRow(`WITH observations AS (
		SELECT rd.selected_upstream_id, rd.actual_upstream_id,
			CAST(rd.actual_cost AS DOUBLE PRECISION) AS actual_cost,
			CASE WHEN rd.actual_outcome='success'
				AND rd.selected_upstream_id>0 AND rd.actual_upstream_id=rd.selected_upstream_id
				AND c.eligible=TRUE AND c.forecast_total_cost>=0
				AND rd.actual_cost>=0 AND rd.forecast_requests>0
			THEN CAST(c.forecast_total_cost AS DOUBLE PRECISION)
				/ NULLIF(CAST(rd.forecast_requests AS DOUBLE PRECISION),0)
			END AS forecast_cost
		FROM route_decisions rd
		LEFT JOIN route_decision_candidates c ON c.decision_id=rd.id
			AND c.upstream_id=rd.selected_upstream_id AND c.selected=TRUE`+where+`
	)
	SELECT COUNT(*), COUNT(forecast_cost),
		COALESCE(SUM(forecast_cost),0),
		COALESCE(SUM(CASE WHEN forecast_cost IS NOT NULL THEN actual_cost ELSE 0 END),0),
		COALESCE(SUM(ABS(actual_cost-forecast_cost)),0),
		COALESCE(SUM(CASE WHEN actual_cost>forecast_cost THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN actual_cost<forecast_cost THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN actual_upstream_id>0 AND selected_upstream_id>0
			AND actual_upstream_id<>selected_upstream_id THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN forecast_cost IS NOT NULL AND actual_cost>0
			THEN ABS(actual_cost-forecast_cost)/actual_cost ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN forecast_cost IS NOT NULL AND actual_cost>0 THEN 1 ELSE 0 END),0)
	FROM observations`, args...)
	if err := row.Scan(&result.TotalDecisions, &result.Samples, &result.ForecastCostSum, &result.ActualCostSum,
		&result.AbsoluteErrorSum, &underestimate, &overestimate, &result.FailoverSamples,
		&percentErrorSum, &result.PercentSamples); err != nil {
		return nil, err
	}
	result.ExcludedSamples = result.TotalDecisions - result.Samples
	if result.Samples > 0 {
		result.MeanAbsoluteError = new(result.AbsoluteErrorSum / float64(result.Samples))
		result.UnderestimateRate = new(float64(underestimate) / float64(result.Samples))
		result.OverestimateRate = new(float64(overestimate) / float64(result.Samples))
	}
	// This MAPE describes the declared audit comparison, not billing accuracy.
	// Zero usage costs remain in absolute errors and rates but not in MAPE.
	if result.PercentSamples > 0 {
		result.MeanAbsolutePercentError = new(percentErrorSum / float64(result.PercentSamples))
	}
	return result, nil
}
