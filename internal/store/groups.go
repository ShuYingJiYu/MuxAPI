package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
)

// Group 虚拟接入点：拥有自己的上游池(经中间表)和接入密钥。
type Group struct {
	ID                   int64       `json:"id"`
	Name                 string      `json:"name"`
	Description          string      `json:"description"`
	MaxMultiplier        *float64    `json:"max_multiplier,omitempty"`
	UpstreamCount        int         `json:"upstream_count"`
	EnabledUpstreamCount int         `json:"enabled_upstream_count"`
	KeyCount             int         `json:"key_count"`
	EnabledKeyCount      int         `json:"enabled_key_count"`
	RecentTotal          int         `json:"recent_total"`
	SuccessRate          int         `json:"success_rate"`
	AvgLatencyMs         int64       `json:"avg_latency_ms"`
	Trend                []HourPoint `json:"trend"`
}

// HourPoint 分组成功率栅栏的一格：代表某 1 小时的请求成功率。
type HourPoint struct {
	Ts       int64   `json:"ts"`
	Status   int     `json:"status"`
	SuccRate float64 `json:"succ_rate"`
	Total    int     `json:"total"`
	Succ     int     `json:"succ"`
}

// AccessKey 接入凭证，绑定到某分组。
type AccessKey struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	GroupID int64  `json:"group_id"`
	Enabled bool   `json:"enabled"`
}

// --- 分组 ---
// ListGroups 保留 raw SQL（复杂多表聚合 + 子查询）
func (s *Store) ListGroups() ([]*Group, error) {
	since := s.timeValue(time.Now().Add(-24 * time.Hour))
	rows, err := s.query(`SELECT
		g.id,g.name,g.description,g.max_multiplier,
		COUNT(DISTINCT gu.upstream_id),
		COUNT(DISTINCT CASE WHEN u.enabled=TRUE AND gu.enabled=TRUE THEN u.id END),
		COUNT(DISTINCT ak.id),
		COUNT(DISTINCT CASE WHEN ak.enabled=TRUE THEN ak.id END),
		COALESCE((SELECT COUNT(*) FROM requests r WHERE r.group_id=g.id AND r.created_at>=?),0),
		COALESCE((SELECT CAST(ROUND(100.0 * SUM(CASE WHEN r.outcome='success' THEN 1 ELSE 0 END) / NULLIF(COUNT(*),0)) AS BIGINT) FROM requests r WHERE r.group_id=g.id AND r.created_at>=?),0),
		COALESCE((SELECT CAST(ROUND(AVG(r.ttft_ms)) AS BIGINT) FROM requests r WHERE r.group_id=g.id AND r.created_at>=? AND r.outcome='success' AND r.ttft_ms>0),0)
		FROM groups g
		LEFT JOIN group_upstreams gu ON gu.group_id=g.id
		LEFT JOIN upstreams u ON u.id=gu.upstream_id
		LEFT JOIN access_keys ak ON ak.group_id=g.id
		GROUP BY g.id,g.name,g.description,g.max_multiplier,g.sort_order
		ORDER BY g.sort_order,g.id`, since, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gs []*Group
	for rows.Next() {
		g := &Group{}
		if err := rows.Scan(
			&g.ID, &g.Name, &g.Description, &g.MaxMultiplier,
			&g.UpstreamCount, &g.EnabledUpstreamCount,
			&g.KeyCount, &g.EnabledKeyCount,
			&g.RecentTotal, &g.SuccessRate, &g.AvgLatencyMs,
		); err != nil {
			return nil, err
		}
		gs = append(gs, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range gs {
		g.Trend = s.groupHourlyTrend(g.ID)
	}
	return gs, nil
}

// groupHourlyTrend 保留 raw SQL（方言差异表达式 hourExpr）
func (s *Store) groupHourlyTrend(groupID int64) []HourPoint {
	const hours = 24
	now := time.Now()
	curHour := now.Truncate(time.Hour)
	start := curHour.Add(-time.Duration(hours-1) * time.Hour)
	type agg struct{ total, succ int }
	buckets := make(map[int64]*agg, hours)
	query := fmt.Sprintf(`SELECT
		%s AS hour, COUNT(*),
		SUM(CASE WHEN outcome='success' THEN 1 ELSE 0 END)
		FROM requests WHERE group_id=? AND created_at>=?
		GROUP BY hour`, s.hourExpr("created_at"))
	rows, err := s.query(query, groupID, s.timeValue(start))
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var h int64
			var total, succ int
			if rows.Scan(&h, &total, &succ) == nil {
				buckets[h] = &agg{total, succ}
			}
		}
	}
	out := make([]HourPoint, hours)
	for i := 0; i < hours; i++ {
		ts := start.Add(time.Duration(i) * time.Hour).Unix()
		p := HourPoint{Ts: ts}
		if a := buckets[ts]; a != nil && a.total > 0 {
			p.Total = a.total
			p.Succ = a.succ
			p.SuccRate = float64(a.succ) / float64(a.total)
			switch {
			case p.SuccRate >= 0.95:
				p.Status = 1
			case p.SuccRate >= 0.80:
				p.Status = 2
			default:
				p.Status = 3
			}
		}
		out[i] = p
	}
	return out
}

// --- 探测结果 ---

func (s *Store) InsertProbe(monitorID int64, status int, latMs int64) error {
	_, err := s.exec(`INSERT INTO probe_results(monitor_id,status,latency_ms,created_at) VALUES(?,?,?,?)`,
		monitorID, status, latMs, s.timeValue(time.Now()))
	return err
}

// MonitorHourlyTrend 保留 raw SQL（方言差异 hourExpr）
func (s *Store) MonitorHourlyTrend(monitorID int64) []HourPoint {
	const hours = 24
	curHour := time.Now().Truncate(time.Hour)
	start := curHour.Add(-time.Duration(hours-1) * time.Hour)
	type agg struct{ total, succ int }
	buckets := make(map[int64]*agg, hours)
	query := fmt.Sprintf(`SELECT
		%s AS hour, COUNT(*),
		SUM(CASE WHEN status=1 THEN 1 ELSE 0 END)
		FROM probe_results WHERE monitor_id=? AND created_at>=?
		GROUP BY hour`, s.hourExpr("created_at"))
	rows, err := s.query(query, monitorID, s.timeValue(start))
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var h int64
			var total, succ int
			if rows.Scan(&h, &total, &succ) == nil {
				buckets[h] = &agg{total, succ}
			}
		}
	}
	out := make([]HourPoint, hours)
	for i := 0; i < hours; i++ {
		ts := start.Add(time.Duration(i) * time.Hour).Unix()
		p := HourPoint{Ts: ts}
		if a := buckets[ts]; a != nil && a.total > 0 {
			p.Total = a.total
			p.Succ = a.succ
			p.SuccRate = float64(a.succ) / float64(a.total)
			switch {
			case p.SuccRate >= 0.95:
				p.Status = 1
			case p.SuccRate >= 0.80:
				p.Status = 2
			default:
				p.Status = 3
			}
		}
		out[i] = p
	}
	return out
}

// MonitorRecent 保留 raw SQL（CAST/ROUND/AVG 聚合 + 方言差异）
func (s *Store) MonitorRecent(monitorID int64) (reqs, succ int, avgMs, lastMs, lastTS int64, lastStatus int) {
	since := s.timeValue(time.Now().Add(-24 * time.Hour))
	s.queryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status=1 THEN 1 ELSE 0 END),0),
		COALESCE(CAST(ROUND(AVG(CASE WHEN status=1 THEN latency_ms END)) AS BIGINT),0)
		FROM probe_results WHERE monitor_id=? AND created_at>=?`, monitorID, since).
		Scan(&reqs, &succ, &avgMs)
	lastQuery := fmt.Sprintf(`SELECT status, latency_ms, %s FROM probe_results
		WHERE monitor_id=? ORDER BY id DESC LIMIT 1`, s.unixExpr("created_at"))
	s.queryRow(lastQuery, monitorID).Scan(&lastStatus, &lastMs, &lastTS)
	return
}

func (s *Store) PruneProbes(keepHours int) (int64, error) {
	if keepHours <= 0 {
		return 0, nil
	}
	cutoff := s.timeValue(time.Now().Add(-time.Duration(keepHours) * time.Hour))
	res, err := s.exec(`DELETE FROM probe_results WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) ForgetProbes(monitorID int64) error {
	_, err := s.exec(`DELETE FROM probe_results WHERE monitor_id=?`, monitorID)
	return err
}

func (s *Store) CreateGroup(name, desc string) (int64, error) {
	return s.CreateGroupWithMaxMultiplier(name, desc, nil)
}

func validMaxMultiplier(value *float64) bool {
	return value == nil || (*value > 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0))
}

func (s *Store) CreateGroupWithMaxMultiplier(name, desc string, maxMultiplier *float64) (int64, error) {
	if !validMaxMultiplier(maxMultiplier) {
		return 0, fmt.Errorf("max multiplier must be greater than zero")
	}
	var maxSort int
	s.gormDB.Model(&GroupModel{}).Select("COALESCE(MAX(sort_order),0)").Scan(&maxSort)
	model := GroupModel{
		Name:          name,
		Description:   desc,
		MaxMultiplier: maxMultiplier,
		SortOrder:     maxSort + 1,
	}
	if err := s.gormDB.Create(&model).Error; err != nil {
		return 0, err
	}
	return model.ID, nil
}

func (s *Store) ReorderGroups(ids []int64) error {
	return s.gormDB.Transaction(func(tx *gorm.DB) error {
		for i, id := range ids {
			if err := tx.Model(&GroupModel{}).Where("id = ?", id).Update("sort_order", i+1).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) UpdateGroup(id int64, name, desc string) error {
	return s.gormDB.Model(&GroupModel{}).Where("id = ?", id).Updates(map[string]any{
		"name": name, "description": desc,
	}).Error
}

func (s *Store) UpdateGroupWithMaxMultiplier(id int64, name, desc string, maxMultiplier *float64) error {
	if !validMaxMultiplier(maxMultiplier) {
		return fmt.Errorf("max multiplier must be greater than zero")
	}
	return s.gormDB.Model(&GroupModel{}).Where("id = ?", id).Updates(map[string]any{
		"name": name, "description": desc, "max_multiplier": maxMultiplier,
	}).Error
}

func (s *Store) DeleteGroup(id int64) error {
	return s.gormDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id = ?", id).Delete(&GroupUpstreamModel{}).Error; err != nil {
			return err
		}
		if err := tx.Where("group_id = ?", id).Delete(&AccessKeyModel{}).Error; err != nil {
			return err
		}
		return tx.Delete(&GroupModel{}, id).Error
	})
}

// --- 组成员（M:N 中间表）---
type Member struct {
	UpstreamID          int64    `json:"upstream_id"`
	Name                string   `json:"name"`
	BaseURL             string   `json:"base_url"`
	Protocol            string   `json:"protocol"`
	BillingType         string   `json:"billing_type"`
	EffectiveMultiplier *float64 `json:"effective_multiplier,omitempty"`
	MultiplierBlocked   bool     `json:"multiplier_blocked"`
	Enabled             bool     `json:"enabled"`
	GroupEnabled        bool     `json:"group_enabled"`
	Priority            int      `json:"priority"`
	Weight              int      `json:"weight"`
	ChannelProbe        bool     `json:"channel_probe"`
}

// ListGroupMembers 保留 raw SQL（复杂 JOIN + 算术表达式）
func (s *Store) ListGroupMembers(groupID int64) ([]*Member, error) {
	rows, err := s.query(`SELECT u.id,u.name,u.base_url,u.protocol,u.billing_type,
		COALESCE(bs.effective_multiplier,bs.group_multiplier)/u.credit_ratio,
		CASE WHEN g.max_multiplier IS NOT NULL
			AND COALESCE(bs.effective_multiplier,bs.group_multiplier)/u.credit_ratio>g.max_multiplier THEN TRUE ELSE FALSE END,
		u.enabled,gu.enabled,gu.priority,gu.weight,u.channel_probe
		FROM upstreams u JOIN group_upstreams gu ON gu.upstream_id=u.id
		JOIN groups g ON g.id=gu.group_id
		LEFT JOIN upstream_billing_status bs ON bs.upstream_id=u.id
		WHERE gu.group_id=? ORDER BY gu.priority ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ms []*Member
	for rows.Next() {
		m := &Member{}
		if err := rows.Scan(&m.UpstreamID, &m.Name, &m.BaseURL, &m.Protocol, &m.BillingType,
			&m.EffectiveMultiplier, &m.MultiplierBlocked, &m.Enabled, &m.GroupEnabled,
			&m.Priority, &m.Weight, &m.ChannelProbe); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}

// AddMember 保留 raw SQL（ON CONFLICT UPSERT）
func (s *Store) AddMember(groupID, upstreamID int64, priority, weight int) error {
	if weight <= 0 {
		weight = 1
	}
	_, err := s.exec(`INSERT INTO group_upstreams(group_id,upstream_id,priority,weight)
		VALUES(?,?,?,?) ON CONFLICT(group_id,upstream_id) DO UPDATE SET priority=?,weight=?`,
		groupID, upstreamID, priority, weight, priority, weight)
	return err
}

func (s *Store) RemoveMember(groupID, upstreamID int64) error {
	return s.gormDB.Where("group_id = ? AND upstream_id = ?", groupID, upstreamID).Delete(&GroupUpstreamModel{}).Error
}

func (s *Store) SetMemberEnabled(groupID, upstreamID int64, enabled bool) error {
	return s.gormDB.Model(&GroupUpstreamModel{}).
		Where("group_id = ? AND upstream_id = ?", groupID, upstreamID).
		Update("enabled", enabled).Error
}

// --- 接入 key ---
func (s *Store) GroupByKey(key string) (int64, bool) {
	var ak AccessKeyModel
	err := s.gormDB.Select("group_id").Where("key = ? AND enabled = ?", key, true).First(&ak).Error
	return ak.GroupID, err == nil
}

func (s *Store) GroupAndKeyByKey(key string) (gid int64, name string, ok bool) {
	var ak AccessKeyModel
	err := s.gormDB.Select("group_id, name").Where("key = ? AND enabled = ?", key, true).First(&ak).Error
	return ak.GroupID, ak.Name, err == nil
}

func (s *Store) CreateKey(name string, groupID int64) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := "sk-mux-" + hex.EncodeToString(b)
	ak := AccessKeyModel{Name: name, Key: key, GroupID: groupID, Enabled: true}
	if err := s.gormDB.Create(&ak).Error; err != nil {
		return "", err
	}
	return key, nil
}

func (s *Store) ListKeys(groupID int64) ([]*AccessKey, error) {
	var models []AccessKeyModel
	q := s.gormDB.Order("id")
	if groupID > 0 {
		q = q.Where("group_id = ?", groupID)
	}
	if err := q.Find(&models).Error; err != nil {
		return nil, err
	}
	ks := make([]*AccessKey, len(models))
	for i, m := range models {
		ks[i] = &AccessKey{ID: m.ID, Name: m.Name, Key: m.Key, GroupID: m.GroupID, Enabled: m.Enabled}
	}
	return ks, nil
}

func (s *Store) SetKeyEnabled(id int64, enabled bool) error {
	return s.gormDB.Model(&AccessKeyModel{}).Where("id = ?", id).Update("enabled", enabled).Error
}

func (s *Store) DeleteKey(id int64) error {
	return s.gormDB.Delete(&AccessKeyModel{}, id).Error
}
