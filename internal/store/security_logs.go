package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

const securityAttention = "(new_source OR new_browser OR after_failures OR frequent)"

// SecurityLogs 列表应用全部筛选；概览仅按 IP 和时间统计，不将后台访问当成登录。
// 时间范围受 15 天保留期约束；翻页返回实际页码，避免清理后出现空的末页。
func (s *Store) SecurityLogs(ctx context.Context, filter SecurityFilter) (SecurityPage, error) {
	page := SecurityPage{Entries: []SecurityLog{}}
	now := time.Now().Unix()
	filter.From = max(filter.From, now-int64(SecurityRetentionDays)*86400)
	if filter.To == 0 || filter.To > now {
		filter.To = now
	}
	page.From, page.To = filter.From, filter.To
	contextQuery := func() *gorm.DB {
		q := s.gormDB.WithContext(ctx).Model(&SecurityLog{}).Where("last_at >= ? AND first_at <= ?", filter.From, filter.To)
		if filter.IP != "" {
			q = q.Where("client_ip = ?", filter.IP)
		}
		return q
	}
	listQuery := func() *gorm.DB {
		q := contextQuery()
		if filter.View == "events" {
			q = q.Where("(kind = ? OR NOT success OR "+securityAttention+")", "login")
		}
		if filter.Kind != "" {
			q = q.Where("kind = ?", filter.Kind)
		}
		switch filter.Result {
		case "success":
			q = q.Where("success = ?", true)
		case "failure":
			q = q.Where("success = ?", false)
		case "attention":
			q = q.Where(securityAttention)
		}
		return q
	}
	if err := listQuery().Count(&page.Total).Error; err != nil {
		return page, err
	}
	if err := contextQuery().Select(`
		COALESCE(SUM(CASE WHEN NOT success THEN count ELSE 0 END), 0) AS failures,
		COUNT(DISTINCT CASE WHEN ` + securityAttention + ` THEN client_ip END) AS attention_ips,
		COALESCE(MAX(CASE WHEN success THEN last_at ELSE 0 END), 0) AS latest_success,
		COALESCE(SUM(CASE WHEN kind = 'login' AND success THEN count ELSE 0 END), 0) AS login_successes,
		COALESCE(SUM(CASE WHEN kind = 'access' THEN count ELSE 0 END), 0) AS management_accesses,
		COALESCE(MAX(CASE WHEN kind = 'login' AND success THEN last_at ELSE 0 END), 0) AS latest_login
	`).Scan(&page.Summary).Error; err != nil {
		return page, err
	}
	size := min(max(filter.Size, 1), 100)
	pageCount := max(1, int((page.Total+int64(size)-1)/int64(size)))
	page.Page, page.Size = min(max(filter.Page, 1), pageCount), size
	offset := (page.Page - 1) * size
	err := listQuery().Order("last_at DESC, id DESC").Offset(offset).Limit(size).Find(&page.Entries).Error
	return page, err
}

// PruneSecurityLogs 固定保留 15 天，使用小批次减少与请求审计的数据库竞争。
func (s *Store) PruneSecurityLogs(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-time.Duration(SecurityRetentionDays) * 24 * time.Hour).Unix()
	ids := s.gormDB.Model(&SecurityLog{}).Select("id").Where("last_at < ?", cutoff).Order("last_at").Limit(5000)
	result := s.gormDB.WithContext(ctx).Where("id IN (?)", ids).Delete(&SecurityLog{})
	return result.RowsAffected, result.Error
}
