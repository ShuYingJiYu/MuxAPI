package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"time"
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
	Trend                []HourPoint `json:"trend"` // 近 24h 按小时分桶的成功率序列（喂给前端栅栏）
}

// HourPoint 分组成功率栅栏的一格：代表某 1 小时的请求成功率。
// Status 复用探测栅栏语义：0 无调用 / 1 正常(≥95%) / 2 降级(≥80%) / 3 故障(<80%)。
type HourPoint struct {
	Ts       int64   `json:"ts"`        // 该小时起始 Unix 秒
	Status   int     `json:"status"`    // 0/1/2/3
	SuccRate float64 `json:"succ_rate"` // 0..1，无调用为 0
	Total    int     `json:"total"`     // 该小时调用数
	Succ     int     `json:"succ"`      // 该小时成功数（2xx-3xx / 探测 status=1）
}

// AccessKey 接入凭证，绑定到某分组：用哪个 key 访问就走哪个分组。
type AccessKey struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	GroupID int64  `json:"group_id"`
	Enabled bool   `json:"enabled"`
}

// --- 分组 ---
func (s *Store) ListGroups() ([]*Group, error) {
	since := s.timeValue(time.Now().Add(-24 * time.Hour))
	rows, err := s.db.Query(`SELECT
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

// groupHourlyTrend 取某分组近 24 小时、按小时分桶的成功率栅栏（24 格，旧→新）。
// 无调用的小时补 status=0 灰格，保证栅栏宽度恒定。
func (s *Store) groupHourlyTrend(groupID int64) []HourPoint {
	const hours = 24
	now := time.Now()
	// 对齐到整点，作为最后一格的起始
	curHour := now.Truncate(time.Hour)
	start := curHour.Add(-time.Duration(hours-1) * time.Hour)
	// 按小时桶聚合：总数 + 成功数
	type agg struct{ total, succ int }
	buckets := make(map[int64]*agg, hours)
	query := fmt.Sprintf(`SELECT
		%s AS hour, COUNT(*),
		SUM(CASE WHEN outcome='success' THEN 1 ELSE 0 END)
		FROM requests WHERE group_id=? AND created_at>=?
		GROUP BY hour`, s.hourExpr("created_at"))
	rows, err := s.db.Query(query, groupID, s.timeValue(start))
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

// --- 探测结果（监控看板按小时统计，落库持久化）---

// InsertProbe 记录一次探测结果。status 为栅栏档位：1正常 2降级 3故障。
func (s *Store) InsertProbe(monitorID int64, status int, latMs int64) error {
	_, err := s.db.Exec(`INSERT INTO probe_results(monitor_id,status,latency_ms,created_at) VALUES(?,?,?,?)`,
		monitorID, status, latMs, s.timeValue(time.Now()))
	return err
}

// MonitorHourlyTrend 取某监控项近 24 小时、按小时分桶的探测成功率栅栏（24 格，旧→新）。
// 与 groupHourlyTrend 同口径：succ=status=1 的次数；空桶补 status=0 灰格。
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
	rows, err := s.db.Query(query, monitorID, s.timeValue(start))
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

// MonitorRecent 近 24h 探测汇总 + 最近一行。
// reqs/succ：总数与成功(status=1)数；avgMs：成功探测的平均延迟。
// lastStatus/lastMs/lastTS：最近一次探测（决定当前 State）；无探测时 reqs=0。
func (s *Store) MonitorRecent(monitorID int64) (reqs, succ int, avgMs, lastMs, lastTS int64, lastStatus int) {
	since := s.timeValue(time.Now().Add(-24 * time.Hour))
	s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status=1 THEN 1 ELSE 0 END),0),
		COALESCE(CAST(ROUND(AVG(CASE WHEN status=1 THEN latency_ms END)) AS BIGINT),0)
		FROM probe_results WHERE monitor_id=? AND created_at>=?`, monitorID, since).
		Scan(&reqs, &succ, &avgMs)
	// 最近一行不受 24h 窗口限制，保证刚重启也能显示上次状态
	lastQuery := fmt.Sprintf(`SELECT status, latency_ms, %s FROM probe_results
		WHERE monitor_id=? ORDER BY id DESC LIMIT 1`, s.unixExpr("created_at"))
	s.db.QueryRow(lastQuery, monitorID).Scan(&lastStatus, &lastMs, &lastTS)
	return
}

// PruneProbes 删除早于 keepHours 小时的探测行，返回删除数。keepHours<=0 关闭清理。
func (s *Store) PruneProbes(keepHours int) (int64, error) {
	if keepHours <= 0 {
		return 0, nil
	}
	cutoff := s.timeValue(time.Now().Add(-time.Duration(keepHours) * time.Hour))
	res, err := s.db.Exec(`DELETE FROM probe_results WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ForgetProbes 删除某监控项的全部探测记录（删监控项时清理）。
func (s *Store) ForgetProbes(monitorID int64) error {
	_, err := s.db.Exec(`DELETE FROM probe_results WHERE monitor_id=?`, monitorID)
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
	var id int64
	err := s.db.QueryRow(`INSERT INTO groups(name,description,max_multiplier,sort_order)
		VALUES(?,?,?,(SELECT COALESCE(MAX(sort_order),0)+1 FROM groups)) RETURNING id`,
		name, desc, maxMultiplier).Scan(&id)
	return id, err
}

// ReorderGroups 按给定 id 顺序持久化分组在管理页中的位置。
func (s *Store) ReorderGroups(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE groups SET sort_order=? WHERE id=?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateGroup(id int64, name, desc string) error {
	_, err := s.db.Exec(`UPDATE groups SET name=?, description=? WHERE id=?`, name, desc, id)
	return err
}

func (s *Store) UpdateGroupWithMaxMultiplier(id int64, name, desc string, maxMultiplier *float64) error {
	if !validMaxMultiplier(maxMultiplier) {
		return fmt.Errorf("max multiplier must be greater than zero")
	}
	_, err := s.db.Exec(`UPDATE groups SET name=?,description=?,max_multiplier=? WHERE id=?`,
		name, desc, maxMultiplier, id)
	return err
}

func (s *Store) DeleteGroup(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM group_upstreams WHERE group_id=?`,
		`DELETE FROM access_keys WHERE group_id=?`,
		`DELETE FROM groups WHERE id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- 组成员（M:N 中间表）---
// Member 组内成员视图：上游基本信息 + 组内 priority/weight。
// Enabled 是上游全局开关（upstreams.enabled，所有分组共享）；
// GroupEnabled 是该上游在本分组内的开关（group_upstreams.enabled）。
// 两者皆为真才参与本组调度，见 ListEnabledByGroup。
type Member struct {
	UpstreamID          int64    `json:"upstream_id"`
	Name                string   `json:"name"`
	BaseURL             string   `json:"base_url"`
	Protocol            string   `json:"protocol"`
	BillingType         string   `json:"billing_type"`
	EffectiveMultiplier *float64 `json:"effective_multiplier,omitempty"`
	MultiplierBlocked   bool     `json:"multiplier_blocked"`
	Enabled             bool     `json:"enabled"`       // 全局开关
	GroupEnabled        bool     `json:"group_enabled"` // 组内开关
	Priority            int      `json:"priority"`
	Weight              int      `json:"weight"`
	ChannelProbe        bool     `json:"channel_probe"` // 兼容旧数据
}

func (s *Store) ListGroupMembers(groupID int64) ([]*Member, error) {
	rows, err := s.db.Query(`SELECT u.id,u.name,u.base_url,u.protocol,u.billing_type,
		COALESCE(bs.effective_multiplier,bs.group_multiplier),
		CASE WHEN g.max_multiplier IS NOT NULL
			AND COALESCE(bs.effective_multiplier,bs.group_multiplier)>g.max_multiplier THEN TRUE ELSE FALSE END,
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

// AddMember 加入/更新组成员的组内策略（UPSERT）。
func (s *Store) AddMember(groupID, upstreamID int64, priority, weight int) error {
	if weight <= 0 {
		weight = 1
	}
	_, err := s.db.Exec(`INSERT INTO group_upstreams(group_id,upstream_id,priority,weight)
		VALUES(?,?,?,?) ON CONFLICT(group_id,upstream_id) DO UPDATE SET priority=?,weight=?`,
		groupID, upstreamID, priority, weight, priority, weight)
	return err
}

func (s *Store) RemoveMember(groupID, upstreamID int64) error {
	_, err := s.db.Exec(`DELETE FROM group_upstreams WHERE group_id=? AND upstream_id=?`, groupID, upstreamID)
	return err
}

// SetMemberEnabled 启停某上游在本分组内的成员资格（不影响其全局开关与其他分组）。
func (s *Store) SetMemberEnabled(groupID, upstreamID int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE group_upstreams SET enabled=? WHERE group_id=? AND upstream_id=?`,
		enabled, groupID, upstreamID)
	return err
}

// --- 接入 key ---
// GroupByKey 根据接入 key 找到其绑定的分组 id（路由核心），仅匹配启用的 key。
func (s *Store) GroupByKey(key string) (int64, bool) {
	var gid int64
	err := s.db.QueryRow(`SELECT group_id FROM access_keys WHERE key=? AND enabled=TRUE`, key).Scan(&gid)
	return gid, err == nil
}

// GroupAndKeyByKey 同 GroupByKey，但一并返回密钥名(供请求记录展示来源客户端)。
func (s *Store) GroupAndKeyByKey(key string) (gid int64, name string, ok bool) {
	err := s.db.QueryRow(`SELECT group_id,COALESCE(name,'') FROM access_keys WHERE key=? AND enabled=TRUE`, key).Scan(&gid, &name)
	return gid, name, err == nil
}

// CreateKey 系统生成 sk-mux-<random> 凭证，返回明文（仅此一次可见）。
func (s *Store) CreateKey(name string, groupID int64) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := "sk-mux-" + hex.EncodeToString(b)
	if _, err := s.db.Exec(`INSERT INTO access_keys(name,key,group_id,enabled) VALUES(?,?,?,TRUE)`,
		name, key, groupID); err != nil {
		return "", err
	}
	return key, nil
}

// ListKeys 返回某分组的密钥（groupID<=0 返回全部）。
func (s *Store) ListKeys(groupID int64) ([]*AccessKey, error) {
	q, args := `SELECT id,name,key,group_id,enabled FROM access_keys`, []any{}
	if groupID > 0 {
		q += ` WHERE group_id=?`
		args = append(args, groupID)
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ks []*AccessKey
	for rows.Next() {
		k := &AccessKey{}
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.GroupID, &k.Enabled); err != nil {
			return nil, err
		}
		ks = append(ks, k)
	}
	return ks, rows.Err()
}

func (s *Store) SetKeyEnabled(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE access_keys SET enabled=? WHERE id=?`, enabled, id)
	return err
}

func (s *Store) DeleteKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM access_keys WHERE id=?`, id)
	return err
}
