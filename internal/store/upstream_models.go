package store

import (
	"time"

	"gorm.io/gorm"
)

// SaveUpstreamModels persists the model list for an upstream. Called after a
// successful /v1/models probe. Existing entries are replaced atomically.
func (s *Store) SaveUpstreamModels(upstreamID int64, models []string) error {
	now := time.Now().Unix()
	return s.gormDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("upstream_id = ?", upstreamID).Delete(&UpstreamModelEntry{}).Error; err != nil {
			return err
		}
		for _, model := range models {
			entry := UpstreamModelEntry{UpstreamID: upstreamID, Model: model, UpdatedAt: now}
			if err := tx.Create(&entry).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadUpstreamModels returns the persisted model list for an upstream.
func (s *Store) LoadUpstreamModels(upstreamID int64) ([]string, error) {
	var entries []UpstreamModelEntry
	if err := s.gormDB.Where("upstream_id = ?", upstreamID).Order("model").Find(&entries).Error; err != nil {
		return nil, err
	}
	models := make([]string, len(entries))
	for i, e := range entries {
		models[i] = e.Model
	}
	return models, nil
}

// LoadAllUpstreamModels returns model lists for all upstreams, keyed by ID.
func (s *Store) LoadAllUpstreamModels() (map[int64][]string, error) {
	var entries []UpstreamModelEntry
	if err := s.gormDB.Order("upstream_id, model").Find(&entries).Error; err != nil {
		return nil, err
	}
	result := make(map[int64][]string)
	for _, e := range entries {
		result[e.UpstreamID] = append(result[e.UpstreamID], e.Model)
	}
	return result, nil
}
