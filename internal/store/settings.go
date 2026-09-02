package store

import "gorm.io/gorm"

// --- 运行时设置（页面可配，key-value）---

func (s *Store) GetSetting(key, def string) string {
	var setting SettingModel
	if err := s.gormDB.Where("key = ?", key).First(&setting).Error; err == nil && setting.Value != "" {
		return setting.Value
	}
	return def
}

func (s *Store) SetSetting(key, value string) error {
	return s.gormDB.Save(&SettingModel{Key: key, Value: value}).Error
}

// SetSettings updates a related group of runtime settings atomically.
func (s *Store) SetSettings(values map[string]string) error {
	return s.gormDB.Transaction(func(tx *gorm.DB) error {
		for key, value := range values {
			if err := tx.Save(&SettingModel{Key: key, Value: value}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
