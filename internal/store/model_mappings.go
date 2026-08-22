package store

import (
	"database/sql"
	"time"
)

// ModelMapping represents a model name mapping rule.
type ModelMapping struct {
	ID           int64      `json:"id"`
	UpstreamID   int64      `json:"upstream_id"`   // 0 = global
	SourceModel  string     `json:"source_model"`  // client-facing model name
	TargetModel  string     `json:"target_model"`  // upstream-facing model name
	MappingType  string     `json:"mapping_type"`  // "static" or "auto"
	FailureCount int        `json:"failure_count"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

const (
	MappingStatic = "static"
	MappingAuto   = "auto"
)

// ListModelMappings returns all model mappings, optionally filtered by upstream.
func (s *Store) ListModelMappings(upstreamID *int64) ([]ModelMapping, error) {
	var rows *sql.Rows
	var err error
	selectCols := `id, upstream_id, source_model, target_model, mapping_type, failure_count, ` +
		s.unixExpr("expires_at") + `, ` + s.unixExpr("created_at") + `, ` + s.unixExpr("updated_at")
	if upstreamID != nil {
		rows, err = s.db.Query(`SELECT `+selectCols+` FROM model_mappings WHERE upstream_id=? ORDER BY upstream_id, source_model`, *upstreamID)
	} else {
		rows, err = s.db.Query(`SELECT ` + selectCols + ` FROM model_mappings ORDER BY upstream_id, source_model`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mappings []ModelMapping
	for rows.Next() {
		var m ModelMapping
		var expiresAt sql.NullInt64
		var createdAt, updatedAt int64
		if err := rows.Scan(&m.ID, &m.UpstreamID, &m.SourceModel, &m.TargetModel,
			&m.MappingType, &m.FailureCount, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if expiresAt.Valid && expiresAt.Int64 > 0 {
			t := time.Unix(expiresAt.Int64, 0)
			m.ExpiresAt = &t
		}
		m.CreatedAt = time.Unix(createdAt, 0)
		m.UpdatedAt = time.Unix(updatedAt, 0)
		mappings = append(mappings, m)
	}
	return mappings, rows.Err()
}

// GetModelMapping retrieves a single mapping by upstream and source model.
// Returns nil if not found (no error).
func (s *Store) GetModelMapping(upstreamID int64, sourceModel string) (*ModelMapping, error) {
	var m ModelMapping
	var expiresAt sql.NullInt64
	var createdAt, updatedAt int64
	selectCols := `id, upstream_id, source_model, target_model, mapping_type, failure_count, ` +
		s.unixExpr("expires_at") + `, ` + s.unixExpr("created_at") + `, ` + s.unixExpr("updated_at")
	err := s.db.QueryRow(`SELECT `+selectCols+` FROM model_mappings WHERE upstream_id=? AND source_model=?`,
		upstreamID, sourceModel).
		Scan(&m.ID, &m.UpstreamID, &m.SourceModel, &m.TargetModel,
			&m.MappingType, &m.FailureCount, &expiresAt, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid && expiresAt.Int64 > 0 {
		t := time.Unix(expiresAt.Int64, 0)
		m.ExpiresAt = &t
	}
	m.CreatedAt = time.Unix(createdAt, 0)
	m.UpdatedAt = time.Unix(updatedAt, 0)
	return &m, nil
}

// UpsertModelMapping creates or updates a model mapping.
func (s *Store) UpsertModelMapping(m *ModelMapping) error {
	now := time.Now()
	var expiresAt any
	if m.ExpiresAt != nil {
		expiresAt = s.timeValue(*m.ExpiresAt)
	}
	err := s.db.QueryRow(`INSERT INTO model_mappings(upstream_id, source_model, target_model, mapping_type, failure_count, expires_at, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(upstream_id, source_model) DO UPDATE SET
			target_model=excluded.target_model,
			mapping_type=excluded.mapping_type,
			failure_count=excluded.failure_count,
			expires_at=excluded.expires_at,
			updated_at=excluded.updated_at
		RETURNING id`,
		m.UpstreamID, m.SourceModel, m.TargetModel, m.MappingType,
		m.FailureCount, expiresAt, s.timeValue(now), s.timeValue(now)).Scan(&m.ID)
	if err != nil {
		return err
	}
	m.CreatedAt = now
	m.UpdatedAt = now
	return nil
}

// DeleteModelMapping removes a mapping by ID.
func (s *Store) DeleteModelMapping(id int64) error {
	_, err := s.db.Exec(`DELETE FROM model_mappings WHERE id=?`, id)
	return err
}

// IncrementMappingFailure atomically increments the failure count for an
// upstream+model pair, creating an auto-mapping if it doesn't exist.
// Returns the new failure count.
func (s *Store) IncrementMappingFailure(upstreamID int64, sourceModel string) (int, error) {
	now := time.Now()
	var count int
	err := s.db.QueryRow(`INSERT INTO model_mappings(upstream_id, source_model, target_model, mapping_type, failure_count, created_at, updated_at)
		VALUES(?, ?, '', 'auto', 1, ?, ?)
		ON CONFLICT(upstream_id, source_model) DO UPDATE SET
			failure_count=model_mappings.failure_count+1,
			updated_at=excluded.updated_at
		RETURNING failure_count`,
		upstreamID, sourceModel, s.timeValue(now), s.timeValue(now)).Scan(&count)
	return count, err
}

// ResolveModelMapping looks up the effective model name for a given upstream
// and source model. Checks upstream-specific first, then global (upstream_id=0).
// Returns the source model unchanged if no mapping exists or if it's expired.
func (s *Store) ResolveModelMapping(upstreamID int64, sourceModel string) (string, bool) {
	nowUnix := time.Now().Unix()
	// Try upstream-specific mapping first
	var target string
	var expiresAt sql.NullInt64
	err := s.db.QueryRow(`SELECT target_model, `+s.unixExpr("expires_at")+` FROM model_mappings
		WHERE upstream_id=? AND source_model=? AND target_model!=''`,
		upstreamID, sourceModel).Scan(&target, &expiresAt)
	if err == nil {
		if expiresAt.Valid && expiresAt.Int64 > 0 && nowUnix > expiresAt.Int64 {
			// Expired, fall through
		} else {
			return target, true
		}
	}
	// Try global mapping
	err = s.db.QueryRow(`SELECT target_model, `+s.unixExpr("expires_at")+` FROM model_mappings
		WHERE upstream_id=0 AND source_model=? AND target_model!=''`,
		sourceModel).Scan(&target, &expiresAt)
	if err == nil {
		if expiresAt.Valid && expiresAt.Int64 > 0 && nowUnix > expiresAt.Int64 {
			return sourceModel, false
		}
		return target, true
	}
	return sourceModel, false
}

// CleanExpiredMappings removes auto-learned mappings that have expired.
func (s *Store) CleanExpiredMappings() (int64, error) {
	result, err := s.db.Exec(`DELETE FROM model_mappings WHERE mapping_type='auto' AND expires_at IS NOT NULL AND expires_at < ?`,
		s.timeValue(time.Now()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
