package store

import (
	"database/sql"
	"time"
)

// PassiveWindow contains durable observations derived from real client
// traffic. It is intentionally separate from probe_results, which represents
// synthetic active checks.
type PassiveWindow struct {
	WindowSeconds int64                 `json:"window_seconds"`
	Since         int64                 `json:"since"`
	Requests      *RequestStats         `json:"requests"`
	RequestHealth PassiveRequestHealth  `json:"request_health"`
	Channels      []PassiveChannelStats `json:"channels"`
}

// PassiveRequestHealth is the request-level SLI denominator for passive
// traffic. Neutral client/model outcomes are visible but excluded from the
// upstream health rate; unknown outcomes remain failures until classified.
type PassiveRequestHealth struct {
	Eligible    int64   `json:"eligible"`
	Successes   int64   `json:"successes"`
	Failures    int64   `json:"failures"`
	Neutral     int64   `json:"neutral"`
	SuccessRate float64 `json:"success_rate"`
}

// PassiveChannelStats groups upstream attempts by actual upstream and mapped
// model. Neutral client-side outcomes are reported separately and excluded
// from the health denominator.
type PassiveChannelStats struct {
	UpstreamID    int64   `json:"upstream_id"`
	UpstreamName  string  `json:"upstream_name"`
	Model         string  `json:"model"`
	Attempts      int64   `json:"attempts"`
	Successes     int64   `json:"successes"`
	Failures      int64   `json:"failures"`
	Neutral       int64   `json:"neutral"`
	SuccessRate   float64 `json:"success_rate"`
	AvgTTFTMs     float64 `json:"avg_ttft_ms"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// PassiveStats returns one durable traffic window. It reads only existing
// request audit tables and never writes or feeds the health manager.
func (s *Store) PassiveStats(since time.Time) (*PassiveWindow, error) {
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}
	window := &PassiveWindow{
		WindowSeconds: int64(time.Since(since).Seconds()),
		Since:         since.Unix(),
		Channels:      []PassiveChannelStats{},
	}
	stats, err := s.RequestStats(RequestFilter{Since: since})
	if err != nil {
		return nil, err
	}
	window.Requests = stats
	requestHealth, err := s.passiveRequestHealth(since)
	if err != nil {
		return nil, err
	}
	window.RequestHealth = requestHealth
	rows, err := s.query(`SELECT a.upstream_id, COALESCE(u.name,''),
		COALESCE(NULLIF(a.mapped_model,''), r.model), COUNT(*),
		COALESCE(SUM(CASE WHEN a.outcome='success' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN a.outcome NOT IN ('success','canceled','client_error','unsupported') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN a.outcome IN ('canceled','client_error','unsupported') THEN 1 ELSE 0 END),0),
		COALESCE(AVG(CASE WHEN a.ttft_ms>0 THEN a.ttft_ms END),0),
		COALESCE(AVG(CASE WHEN a.duration_ms>0 THEN a.duration_ms END),0)
		FROM request_attempts a
		JOIN requests r ON r.request_id=a.request_id
		LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE r.created_at >= ?
			AND a.upstream_id > 0
			AND a.error_source <> 'gateway'
			AND (a.status <> 0 OR a.error_source = 'upstream')
		GROUP BY a.upstream_id, u.name, COALESCE(NULLIF(a.mapped_model,''), r.model)
		ORDER BY COUNT(*) DESC, a.upstream_id, COALESCE(NULLIF(a.mapped_model,''), r.model)
		LIMIT 500`, s.timeValue(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item PassiveChannelStats
		var avgTTFT, avgDuration sql.NullFloat64
		if err := rows.Scan(&item.UpstreamID, &item.UpstreamName, &item.Model, &item.Attempts,
			&item.Successes, &item.Failures, &item.Neutral, &avgTTFT, &avgDuration); err != nil {
			return nil, err
		}
		item.AvgTTFTMs, item.AvgDurationMs = avgTTFT.Float64, avgDuration.Float64
		denominator := item.Successes + item.Failures
		if denominator > 0 {
			item.SuccessRate = float64(item.Successes) / float64(denominator)
		}
		window.Channels = append(window.Channels, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return window, nil
}

func (s *Store) passiveRequestHealth(since time.Time) (PassiveRequestHealth, error) {
	rows, err := s.query(`SELECT outcome, COUNT(*) FROM requests WHERE created_at >= ? GROUP BY outcome`, s.timeValue(since))
	if err != nil {
		return PassiveRequestHealth{}, err
	}
	defer rows.Close()
	var health PassiveRequestHealth
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			return PassiveRequestHealth{}, err
		}
		switch outcome {
		case "success":
			health.Successes += count
		case "canceled", "client_error", "unsupported":
			health.Neutral += count
		default:
			health.Failures += count
		}
	}
	if err := rows.Err(); err != nil {
		return PassiveRequestHealth{}, err
	}
	health.Eligible = health.Successes + health.Failures
	if health.Eligible > 0 {
		health.SuccessRate = float64(health.Successes) / float64(health.Eligible)
	}
	return health, nil
}
