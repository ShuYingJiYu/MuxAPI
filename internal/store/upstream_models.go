package store

import "time"

// SaveUpstreamModels persists the model list for an upstream. Called after a
// successful /v1/models probe. Existing entries are replaced atomically.
func (s *Store) SaveUpstreamModels(upstreamID int64, models []string) error {
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM upstream_models WHERE upstream_id=?`, upstreamID); err != nil {
		return err
	}
	for _, model := range models {
		if _, err := tx.Exec(`INSERT INTO upstream_models(upstream_id, model, updated_at) VALUES(?,?,?)`,
			upstreamID, model, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadUpstreamModels returns the persisted model list for an upstream.
func (s *Store) LoadUpstreamModels(upstreamID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT model FROM upstream_models WHERE upstream_id=? ORDER BY model`, upstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

// LoadAllUpstreamModels returns model lists for all upstreams, keyed by ID.
func (s *Store) LoadAllUpstreamModels() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT upstream_id, model FROM upstream_models ORDER BY upstream_id, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64][]string)
	for rows.Next() {
		var id int64
		var m string
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		result[id] = append(result[id], m)
	}
	return result, rows.Err()
}
