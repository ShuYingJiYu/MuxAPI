package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func securityText(value string, limit int) string {
	// 限制检查的原始长度，避免用大量不可见字符消耗日志线程。
	if len(value) > limit*4 {
		value = value[:limit*4]
	}
	var out strings.Builder
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			continue
		}
		out.WriteRune(char)
		limit--
		if limit == 0 {
			break
		}
	}
	return out.String()
}

// EnqueueSecurity 不阻塞管理请求；失败写入和队列溢出均计入可见丢弃数。
func (s *Store) EnqueueSecurity(record SecurityLog) bool {
	record.ID, record.Count = 0, 1
	record.NewSource, record.NewBrowser, record.AfterFailures, record.Frequent = false, false, false, false
	record.LastAt = record.FirstAt
	record.ClientIP = securityText(record.ClientIP, 45)
	record.PeerIP = securityText(record.PeerIP, 45)
	record.UserAgent = securityText(record.UserAgent, 256)
	record.Endpoint = securityText(record.Endpoint, 120)
	record.Method = securityText(record.Method, 16)
	record.Kind = securityText(record.Kind, 16)
	key, _ := json.Marshal([]any{record.FirstAt / 60, record.Kind, record.Success,
		record.ClientIP, record.PeerIP, record.Forwarded, record.UserAgent, record.Method, record.Endpoint})
	hash := sha256.Sum256(key)
	record.AggregationKey = hex.EncodeToString(hash[:])
	s.securityMu.RLock()
	defer s.securityMu.RUnlock()
	if !s.securityClosed {
		select {
		case s.securityQueue <- securityWrite{record: &record}:
			return true
		default:
		}
	}
	s.dropSecurity(1)
	return false
}

func (s *Store) dropSecurity(count int64) {
	previous := s.securityDrops.Add(uint64(count)) - uint64(count)
	if previous == 0 {
		slog.Error("security audit incomplete; inspect security log status")
	}
}

func (s *Store) SecurityLogStatus() (uint64, bool) {
	return s.securityDrops.Load(), s.securityWriteFailed.Load()
}

func (s *Store) FlushSecurity(ctx context.Context) error {
	done := make(chan struct{})
	s.securityMu.RLock()
	if s.securityClosed {
		s.securityMu.RUnlock()
		return errors.New("security writer closed")
	}
	select {
	case s.securityQueue <- securityWrite{barrier: done}:
		s.securityMu.RUnlock()
	case <-ctx.Done():
		s.securityMu.RUnlock()
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) runSecurityWriter() {
	defer close(s.securityDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	pending := make(map[string]*SecurityLog)
	// 每种验证结果每分钟最多接纳 128 个不同组合，成功访问保留独立额度。
	// 队列限制内存，此限制同时约束持续攻击期间的数据库增长。
	admitted := [2]map[string]struct{}{{}, {}}
	var admissionMinute int64 = -1
	flush := func() {
		if len(pending) == 0 {
			return
		}
		rows := make([]*SecurityLog, 0, len(pending))
		var count int64
		for _, row := range pending {
			rows = append(rows, row)
			count += row.Count
		}
		sort.Slice(rows, func(i, j int) bool {
			// 先写失败，使同一批内的「失败后成功」也可识别。
			if rows[i].Success != rows[j].Success {
				return !rows[i].Success
			}
			if rows[i].FirstAt != rows[j].FirstAt {
				return rows[i].FirstAt < rows[j].FirstAt
			}
			return rows[i].AggregationKey < rows[j].AggregationKey
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.writeSecurityBatch(ctx, rows)
		cancel()
		if err != nil {
			s.dropSecurity(count)
			// 不输出数据库错误正文，避免驱动把参数（UA 等）复制到运行日志。
			if !s.securityWriteFailed.Swap(true) {
				slog.Error("security audit database write failed")
			}
		} else {
			s.securityWriteFailed.Store(false)
		}
		clear(pending)
	}
	for {
		select {
		case <-ticker.C:
			flush()
		case item, open := <-s.securityQueue:
			if !open {
				flush()
				return
			}
			if item.barrier != nil {
				flush()
				close(item.barrier)
				continue
			}
			row := item.record
			if minute := row.FirstAt / 60; minute > admissionMinute {
				clear(admitted[0])
				clear(admitted[1])
				admissionMinute = minute
			}
			result := 0
			if row.Success {
				result = 1
			}
			if _, exists := admitted[result][row.AggregationKey]; !exists {
				if len(admitted[result]) >= 128 {
					s.dropSecurity(row.Count)
					continue
				}
				admitted[result][row.AggregationKey] = struct{}{}
			}
			if prior := pending[row.AggregationKey]; prior != nil {
				prior.Count += row.Count
				prior.FirstAt = min(prior.FirstAt, row.FirstAt)
				prior.LastAt = max(prior.LastAt, row.LastAt)
			} else if len(pending) < 128 {
				pending[row.AggregationKey] = row
			} else {
				// 限制每批不同来源，避免变换 UA/IP 的日志洪水产生无限写入。
				s.dropSecurity(row.Count)
			}
		}
	}
}

func (s *Store) writeSecurityBatch(ctx context.Context, rows []*SecurityLog) error {
	return s.gormDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			window := (row.LastAt/60 - 4) * 60
			var failures int64
			if err := tx.Model(&SecurityLog{}).Where("client_ip = ? AND success = ? AND last_at >= ? AND first_at <= ?",
				row.ClientIP, false, window, row.LastAt).Select("COALESCE(SUM(count), 0)").Scan(&failures).Error; err != nil {
				return err
			}
			if row.Success {
				var sourceID, browserID int64
				history := func() *gorm.DB {
					return tx.Model(&SecurityLog{}).Where("client_ip = ? AND success = ? AND last_at >= ? AND first_at <= ?",
						row.ClientIP, true, row.FirstAt-int64(SecurityRetentionDays)*86400, row.LastAt)
				}
				if err := history().Select("id").Limit(1).Scan(&sourceID).Error; err != nil {
					return err
				}
				if err := history().Where("user_agent = ?", row.UserAgent).Select("id").Limit(1).Scan(&browserID).Error; err != nil {
					return err
				}
				row.NewSource = sourceID == 0
				row.NewBrowser = sourceID != 0 && browserID == 0
				row.AfterFailures = failures > 0
			} else {
				row.Frequent = failures+row.Count >= 10
			}
			updates := map[string]any{
				"count":    gorm.Expr("security_logs.count + excluded.count"),
				"first_at": gorm.Expr("CASE WHEN security_logs.first_at < excluded.first_at THEN security_logs.first_at ELSE excluded.first_at END"),
				"last_at":  gorm.Expr("CASE WHEN security_logs.last_at > excluded.last_at THEN security_logs.last_at ELSE excluded.last_at END"),
			}
			for _, flag := range []string{"new_source", "new_browser", "after_failures", "frequent"} {
				updates[flag] = gorm.Expr("security_logs." + flag + " OR excluded." + flag)
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "aggregation_key"}},
				DoUpdates: clause.Assignments(updates)}).Create(row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
