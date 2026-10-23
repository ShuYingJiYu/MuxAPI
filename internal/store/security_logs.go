package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// SecurityLogs 查询的时间范围总是受 15 天保留期约束；摘要与列表使用同一筛选。
func (s *Store) SecurityLogs(ctx context.Context, filter SecurityFilter) (SecurityPage, error) {
	page := SecurityPage{Entries: []SecurityLog{}}
	now := time.Now().Unix()
	filter.From = max(filter.From, now-int64(SecurityRetentionDays)*86400)
	if filter.To == 0 || filter.To > now {
		filter.To = now
	}
	base := func() *gorm.DB {
		q := s.gormDB.WithContext(ctx).Model(&SecurityLog{}).Where("last_at >= ? AND first_at <= ?", filter.From, filter.To)
		if filter.IP != "" {
			q = q.Where("client_ip = ?", filter.IP)
		}
		switch filter.Result {
		case "success":
			q = q.Where("success = ?", true)
		case "failure":
			q = q.Where("success = ?", false)
		case "attention":
			q = q.Where("new_source OR new_browser OR after_failures OR frequent")
		}
		return q
	}
	if err := base().Count(&page.Total).Error; err != nil {
		return page, err
	}
	if err := base().Where("success = ?", false).Select("COALESCE(SUM(count), 0)").Scan(&page.Summary.Failures).Error; err != nil {
		return page, err
	}
	if err := base().Where("new_source OR new_browser OR after_failures OR frequent").Distinct("client_ip").Count(&page.Summary.AttentionIPs).Error; err != nil {
		return page, err
	}
	if err := base().Where("success = ?", true).Select("COALESCE(MAX(last_at), 0)").Scan(&page.Summary.LatestSuccess).Error; err != nil {
		return page, err
	}
	size := min(max(filter.Size, 1), 100)
	offset := (min(max(filter.Page, 1), 10000) - 1) * size
	err := base().Order("last_at DESC, id DESC").Offset(offset).Limit(size).Find(&page.Entries).Error
	return page, err
}

// PruneSecurityLogs 固定保留 15 天，使用小批次减少与请求审计的数据库竞争。
func (s *Store) PruneSecurityLogs(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-time.Duration(SecurityRetentionDays) * 24 * time.Hour).Unix()
	ids := s.gormDB.Model(&SecurityLog{}).Select("id").Where("last_at < ?", cutoff).Order("last_at").Limit(5000)
	result := s.gormDB.WithContext(ctx).Where("id IN (?)", ids).Delete(&SecurityLog{})
	return result.RowsAffected, result.Error
}
