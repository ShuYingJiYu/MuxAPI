package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

// Group 虚拟接入点：拥有自己的上游池(经中间表)和接入密钥。
type Group struct {
	ID                   int64  `json:"id"`
	Name                 string `json:"name"`
	Description          string `json:"description"`
	UpstreamCount        int    `json:"upstream_count"`
	EnabledUpstreamCount int    `json:"enabled_upstream_count"`
	KeyCount             int    `json:"key_count"`
	EnabledKeyCount      int    `json:"enabled_key_count"`
	RecentTotal          int    `json:"recent_total"`
	SuccessRate          int    `json:"success_rate"`
	AvgLatencyMs         int64  `json:"avg_latency_ms"`
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

// Monitor 监控项：对某渠道的某个模型主动探测。凭证用渠道自带的 api_key。
// UpstreamName/BaseURL/APIKey 为 JOIN 出的渠道视图字段（探测/展示用）。
// Stream/ProbeText/MaxTokens/IntervalSec/Path 为可配探测参数，
// 空字符串/0 一律表示「沿用全局默认」（见 monitor.Prober）。
type Monitor struct {
	ID           int64  `json:"id"`
	UpstreamID   int64  `json:"upstream_id"`
	Model        string `json:"model"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	Stream       bool   `json:"stream"`       // 探测是否走流式（请求体加 stream:true）
	ProbeText    string `json:"probe_text"`   // 探测消息内容，空=默认 "hi"
	MaxTokens    int    `json:"max_tokens"`   // 探测 max_tokens，0=默认 1
	IntervalSec  int    `json:"interval_sec"` // 该项探测周期(秒)，0=用全局
	Path         string `json:"path"`         // 该项探测端点，空=用全局
	UpstreamName string `json:"upstream_name,omitempty"`
	BaseURL      string `json:"-"`
	APIKey       string `json:"-"`
	Proxy        string `json:"-"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(schema); err != nil {
		return nil, err
	}
	// 迁移：旧库 upstreams 补 proxy 列（已存在则忽略报错）
	db.Exec(`ALTER TABLE upstreams ADD COLUMN proxy TEXT NOT NULL DEFAULT ''`)
	// 迁移：旧库 group_upstreams 补 enabled 列（组内成员开关，默认启用）
	db.Exec(`ALTER TABLE group_upstreams ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1`)
	// 迁移：旧库 monitors 补可配探测列（空/0 表示沿用全局默认）
	db.Exec(`ALTER TABLE monitors ADD COLUMN stream INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE monitors ADD COLUMN probe_text TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE monitors ADD COLUMN max_tokens INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE monitors ADD COLUMN interval_sec INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE monitors ADD COLUMN path TEXT NOT NULL DEFAULT ''`)
	// 迁移：旧库 monitors 补 sort 列（拖拽排序权重，0=未排过按 id）
	db.Exec(`ALTER TABLE monitors ADD COLUMN sort INTEGER NOT NULL DEFAULT 0`)
	// 迁移：旧库 logs 补 model 列（请求记录按模型维度展示，旧行为空）
	db.Exec(`ALTER TABLE logs ADD COLUMN model TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE logs ADD COLUMN endpoint TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE logs ADD COLUMN key_name TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE logs ADD COLUMN retries INTEGER NOT NULL DEFAULT 0`)
	return &Store{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS groups (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS upstreams (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	name     TEXT NOT NULL,
	base_url TEXT NOT NULL,
	api_key  TEXT NOT NULL,
	proxy    TEXT NOT NULL DEFAULT '',
	enabled  INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS group_upstreams (
	group_id    INTEGER NOT NULL,
	upstream_id INTEGER NOT NULL,
	priority    INTEGER NOT NULL DEFAULT 50,
	weight      INTEGER NOT NULL DEFAULT 1,
	enabled     INTEGER NOT NULL DEFAULT 1,
	PRIMARY KEY (group_id, upstream_id)
);
CREATE TABLE IF NOT EXISTS access_keys (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	name     TEXT NOT NULL DEFAULT '',
	key      TEXT NOT NULL UNIQUE,
	group_id INTEGER NOT NULL,
	enabled  INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS monitors (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream_id  INTEGER NOT NULL,
	model        TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	enabled      INTEGER NOT NULL DEFAULT 1,
	stream       INTEGER NOT NULL DEFAULT 0,
	probe_text   TEXT NOT NULL DEFAULT '',
	max_tokens   INTEGER NOT NULL DEFAULT 0,
	interval_sec INTEGER NOT NULL DEFAULT 0,
	path         TEXT NOT NULL DEFAULT '',
	sort         INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS logs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	group_id    INTEGER NOT NULL,
	upstream_id INTEGER NOT NULL,
	status      INTEGER NOT NULL,
	latency_ms  INTEGER NOT NULL,
	created_at  INTEGER NOT NULL,
	model       TEXT NOT NULL DEFAULT '',
	endpoint    TEXT NOT NULL DEFAULT '',
	key_name    TEXT NOT NULL DEFAULT '',
	retries     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS probe_results (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	monitor_id INTEGER NOT NULL,
	status     INTEGER NOT NULL,   -- 栅栏档位：1正常 2降级 3故障
	latency_ms INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_probe_mon_time ON probe_results(monitor_id, created_at);`

// --- 上游全局池 ---

// scanUps 扫描含组内视图 priority/weight 的行（JOIN 查询用）。
func scanUps(rows *sql.Rows) ([]*upstream.Upstream, error) {
	defer rows.Close()
	var list []*upstream.Upstream
	for rows.Next() {
		u := &upstream.Upstream{}
		if err := rows.Scan(&u.ID, &u.Name, &u.BaseURL, &u.APIKey, &u.Proxy, &u.Enabled, &u.Priority, &u.Weight); err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

// ListEnabledByGroup 返回某分组下启用的上游，JOIN 中间表填充组内 priority/weight，
// 按组内优先级升序（调度用）。
func (s *Store) ListEnabledByGroup(groupID int64) ([]*upstream.Upstream, error) {
	rows, err := s.db.Query(`SELECT u.id,u.name,u.base_url,u.api_key,u.proxy,u.enabled,gu.priority,gu.weight
		FROM upstreams u JOIN group_upstreams gu ON gu.upstream_id=u.id
		WHERE gu.group_id=? AND u.enabled=1 AND gu.enabled=1 ORDER BY gu.priority ASC`, groupID)
	if err != nil {
		return nil, err
	}
	return scanUps(rows)
}

// List 返回全部上游(含停用)，priority/weight 置 0（全局池无组内语义），供探测与后台管理。
func (s *Store) List() ([]*upstream.Upstream, error) {
	rows, err := s.db.Query(`SELECT id,name,base_url,api_key,proxy,enabled,0,0 FROM upstreams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanUps(rows)
}

// Get 按 id 取单个上游（含完整 api_key，供连通测试用）。
func (s *Store) Get(id int64) (*upstream.Upstream, error) {
	u := &upstream.Upstream{}
	err := s.db.QueryRow(`SELECT id,name,base_url,api_key,proxy,enabled FROM upstreams WHERE id=?`, id).
		Scan(&u.ID, &u.Name, &u.BaseURL, &u.APIKey, &u.Proxy, &u.Enabled)
	return u, err
}

func (s *Store) Create(u *upstream.Upstream) error {
	_, err := s.db.Exec(`INSERT INTO upstreams(name,base_url,api_key,proxy,enabled) VALUES(?,?,?,?,?)`,
		u.Name, u.BaseURL, u.APIKey, u.Proxy, u.Enabled)
	return err
}

func (s *Store) Update(u *upstream.Upstream) error {
	if u.APIKey == "" { // 留空则不改凭证（对齐后台「留空则不修改」语义）
		_, err := s.db.Exec(`UPDATE upstreams SET name=?,base_url=?,proxy=?,enabled=? WHERE id=?`,
			u.Name, u.BaseURL, u.Proxy, u.Enabled, u.ID)
		return err
	}
	_, err := s.db.Exec(`UPDATE upstreams SET name=?,base_url=?,api_key=?,proxy=?,enabled=? WHERE id=?`,
		u.Name, u.BaseURL, u.APIKey, u.Proxy, u.Enabled, u.ID)
	return err
}

func (s *Store) Delete(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM group_upstreams WHERE upstream_id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM monitors WHERE upstream_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM upstreams WHERE id=?`, id)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// --- 监控项 ---

// scanMonitors 扫描含渠道视图(name/base_url/api_key)的 JOIN 行。
func scanMonitors(rows *sql.Rows) ([]*Monitor, error) {
	defer rows.Close()
	var ms []*Monitor
	for rows.Next() {
		m := &Monitor{}
		if err := rows.Scan(&m.ID, &m.UpstreamID, &m.Model, &m.Name, &m.Enabled,
			&m.Stream, &m.ProbeText, &m.MaxTokens, &m.IntervalSec, &m.Path,
			&m.UpstreamName, &m.BaseURL, &m.APIKey, &m.Proxy); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}

const monitorJoin = `SELECT m.id,m.upstream_id,m.model,m.name,m.enabled,m.stream,m.probe_text,m.max_tokens,m.interval_sec,m.path,u.name,u.base_url,u.api_key,u.proxy
	FROM monitors m JOIN upstreams u ON u.id=m.upstream_id`

// ListMonitors 全部监控项；enabledOnly 时只返回启用且渠道也启用的（探测用）。
func (s *Store) ListMonitors(enabledOnly bool) ([]*Monitor, error) {
	q := monitorJoin
	if enabledOnly {
		q += ` WHERE m.enabled=1 AND u.enabled=1`
	}
	rows, err := s.db.Query(q + ` ORDER BY m.sort, m.id`)
	if err != nil {
		return nil, err
	}
	return scanMonitors(rows)
}

func (s *Store) GetMonitor(id int64) (*Monitor, error) {
	rows, err := s.db.Query(monitorJoin+` WHERE m.id=?`, id)
	if err != nil {
		return nil, err
	}
	ms, err := scanMonitors(rows)
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	return ms[0], nil
}

func (s *Store) CreateMonitor(m *Monitor) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO monitors(upstream_id,model,name,enabled,stream,probe_text,max_tokens,interval_sec,path)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		m.UpstreamID, m.Model, m.Name, m.Enabled, m.Stream, m.ProbeText, m.MaxTokens, m.IntervalSec, m.Path)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ReorderMonitors 按给定 id 顺序写入 sort 权重（从 1 起，下标即权重）。
// 一次事务内全量更新；未出现在 ids 中的监控项保持原 sort，会排在已排项之后或之间。
func (s *Store) ReorderMonitors(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE monitors SET sort=? WHERE id=?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) MonitoredModels(upstreamID int64) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT model FROM monitors WHERE upstream_id=?`, upstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err == nil {
			set[m] = true
		}
	}
	return set, rows.Err()
}

// BatchCreateMonitors 为某上游的一批模型批量建监控，已存在 (upstream,model) 的跳过。
// tmpl 提供共享探测参数（model 字段被逐个模型覆盖）。返回 (created, skipped)。
func (s *Store) BatchCreateMonitors(upstreamID int64, models []string, tmpl Monitor) (int, int, error) {
	existing, err := s.MonitoredModels(upstreamID)
	if err != nil {
		return 0, 0, err
	}
	created, skipped := 0, 0
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || existing[model] {
			skipped++
			continue
		}
		m := tmpl // 复制模板
		m.UpstreamID = upstreamID
		m.Model = model
		if _, err := s.CreateMonitor(&m); err != nil {
			return created, skipped, err
		}
		existing[model] = true // 防同批重复
		created++
	}
	return created, skipped, nil
}

func (s *Store) UpdateMonitor(m *Monitor) error {
	_, err := s.db.Exec(`UPDATE monitors SET upstream_id=?,model=?,name=?,enabled=?,stream=?,probe_text=?,max_tokens=?,interval_sec=?,path=? WHERE id=?`,
		m.UpstreamID, m.Model, m.Name, m.Enabled, m.Stream, m.ProbeText, m.MaxTokens, m.IntervalSec, m.Path, m.ID)
	return err
}

func (s *Store) DeleteMonitor(id int64) error {
	_, err := s.db.Exec(`DELETE FROM monitors WHERE id=?`, id)
	return err
}

// --- 分组 ---
func (s *Store) ListGroups() ([]*Group, error) {
	since := time.Now().Add(-24 * time.Hour).Unix()
	rows, err := s.db.Query(`SELECT
		g.id,g.name,g.description,
		COUNT(DISTINCT gu.upstream_id),
		COUNT(DISTINCT CASE WHEN u.enabled=1 AND gu.enabled=1 THEN u.id END),
		COUNT(DISTINCT ak.id),
		COUNT(DISTINCT CASE WHEN ak.enabled=1 THEN ak.id END),
		COALESCE((SELECT COUNT(*) FROM logs l WHERE l.group_id=g.id AND l.created_at>=?),0),
		COALESCE((SELECT ROUND(100.0 * SUM(CASE WHEN l.status BETWEEN 200 AND 399 THEN 1 ELSE 0 END) / COUNT(*)) FROM logs l WHERE l.group_id=g.id AND l.created_at>=?),0),
		COALESCE((SELECT ROUND(AVG(l.latency_ms)) FROM logs l WHERE l.group_id=g.id AND l.created_at>=?),0)
		FROM groups g
		LEFT JOIN group_upstreams gu ON gu.group_id=g.id
		LEFT JOIN upstreams u ON u.id=gu.upstream_id
		LEFT JOIN access_keys ak ON ak.group_id=g.id
		GROUP BY g.id,g.name,g.description
		ORDER BY g.id`, since, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gs []*Group
	for rows.Next() {
		g := &Group{}
		if err := rows.Scan(
			&g.ID, &g.Name, &g.Description,
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
	rows, err := s.db.Query(`SELECT
		(created_at/3600)*3600 AS hour, COUNT(*),
		SUM(CASE WHEN status BETWEEN 200 AND 399 THEN 1 ELSE 0 END)
		FROM logs WHERE group_id=? AND created_at>=?
		GROUP BY hour`, groupID, start.Unix())
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
		monitorID, status, latMs, time.Now().Unix())
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
	rows, err := s.db.Query(`SELECT
		(created_at/3600)*3600 AS hour, COUNT(*),
		SUM(CASE WHEN status=1 THEN 1 ELSE 0 END)
		FROM probe_results WHERE monitor_id=? AND created_at>=?
		GROUP BY hour`, monitorID, start.Unix())
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
	since := time.Now().Add(-24 * time.Hour).Unix()
	s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status=1 THEN 1 ELSE 0 END),0),
		COALESCE(CAST(ROUND(AVG(CASE WHEN status=1 THEN latency_ms END)) AS INTEGER),0)
		FROM probe_results WHERE monitor_id=? AND created_at>=?`, monitorID, since).
		Scan(&reqs, &succ, &avgMs)
	// 最近一行不受 24h 窗口限制，保证刚重启也能显示上次状态
	s.db.QueryRow(`SELECT status, latency_ms, created_at FROM probe_results
		WHERE monitor_id=? ORDER BY id DESC LIMIT 1`, monitorID).
		Scan(&lastStatus, &lastMs, &lastTS)
	return
}

// PruneProbes 删除早于 keepHours 小时的探测行，返回删除数。keepHours<=0 关闭清理。
func (s *Store) PruneProbes(keepHours int) (int64, error) {
	if keepHours <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(keepHours) * time.Hour).Unix()
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
	res, err := s.db.Exec(`INSERT INTO groups(name,description) VALUES(?,?)`, name, desc)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateGroup(id int64, name, desc string) error {
	_, err := s.db.Exec(`UPDATE groups SET name=?, description=? WHERE id=?`, name, desc, id)
	return err
}

func (s *Store) DeleteGroup(id int64) error {
	for _, q := range []string{
		`DELETE FROM group_upstreams WHERE group_id=?`,
		`DELETE FROM access_keys WHERE group_id=?`,
		`DELETE FROM groups WHERE id=?`,
	} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// --- 组成员（M:N 中间表）---
// Member 组内成员视图：上游基本信息 + 组内 priority/weight。
// Enabled 是上游全局开关（upstreams.enabled，所有分组共享）；
// GroupEnabled 是该上游在本分组内的开关（group_upstreams.enabled）。
// 两者皆为真才参与本组调度，见 ListEnabledByGroup。
type Member struct {
	UpstreamID   int64  `json:"upstream_id"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	Enabled      bool   `json:"enabled"`       // 全局开关
	GroupEnabled bool   `json:"group_enabled"` // 组内开关
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
}

func (s *Store) ListGroupMembers(groupID int64) ([]*Member, error) {
	rows, err := s.db.Query(`SELECT u.id,u.name,u.base_url,u.enabled,gu.enabled,gu.priority,gu.weight
		FROM upstreams u JOIN group_upstreams gu ON gu.upstream_id=u.id
		WHERE gu.group_id=? ORDER BY gu.priority ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ms []*Member
	for rows.Next() {
		m := &Member{}
		if err := rows.Scan(&m.UpstreamID, &m.Name, &m.BaseURL, &m.Enabled, &m.GroupEnabled, &m.Priority, &m.Weight); err != nil {
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
	err := s.db.QueryRow(`SELECT group_id FROM access_keys WHERE key=? AND enabled=1`, key).Scan(&gid)
	return gid, err == nil
}

// GroupAndKeyByKey 同 GroupByKey，但一并返回密钥名(供请求记录展示来源客户端)。
func (s *Store) GroupAndKeyByKey(key string) (gid int64, name string, ok bool) {
	err := s.db.QueryRow(`SELECT group_id,COALESCE(name,'') FROM access_keys WHERE key=? AND enabled=1`, key).Scan(&gid, &name)
	return gid, name, err == nil
}

// CreateKey 系统生成 sk-mux-<random> 凭证，返回明文（仅此一次可见）。
func (s *Store) CreateKey(name string, groupID int64) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := "sk-mux-" + hex.EncodeToString(b)
	if _, err := s.db.Exec(`INSERT INTO access_keys(name,key,group_id,enabled) VALUES(?,?,?,1)`,
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

// --- 调用日志 ---

// LogEntry 一条转发日志（JOIN 出上游名、分组名供展示）。
type LogEntry struct {
	ID           int64  `json:"id"`
	GroupID      int64  `json:"group_id"`
	GroupName    string `json:"group_name"`
	UpstreamID   int64  `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	Model        string `json:"model"`  // 请求模型；旧数据/解析失败为空
	Status       int    `json:"status"` // HTTP 状态码；网络失败为 0
	LatencyMs    int64  `json:"latency_ms"`
	CreatedAt    int64  `json:"created_at"`
	Endpoint     string `json:"endpoint"` // 请求端点路径，如 /v1/messages；旧数据为空
	KeyName      string `json:"key_name"` // 命中的接入密钥名；旧数据为空
	Retries      int    `json:"retries"`  // 本次落库时已重试次数(0=首次命中)
}

// Log 记一条转发调用日志（异步友好：忽略写入错误，不阻塞转发）。
func (s *Store) Log(groupID, upstreamID int64, model, endpoint, keyName string, retries, status int, latencyMs int64) {
	s.db.Exec(`INSERT INTO logs(group_id,upstream_id,model,endpoint,key_name,retries,status,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		groupID, upstreamID, model, endpoint, keyName, retries, status, latencyMs, time.Now().Unix())
}

// LogPage 一页日志 + 游标分页元信息。
type LogPage struct {
	Entries    []*LogEntry `json:"entries"`
	HasMore    bool        `json:"has_more"`
	NextCursor int64       `json:"next_cursor"` // 下一页传 before=此值；0=没有更多
}

// ListLogsPage 游标分页 + 服务端筛选。
// beforeID<=0 从最新开始；否则取 id<beforeID 的更旧一页（倒序）。
// model/group 非空按精确匹配；statusClass: "ok"=2xx/3xx, "fail"=其余(含网络失败0)，空=不限。
// 多取一条判断 hasMore，并以本页最后一条 id 作为下一页游标。
func (s *Store) ListLogsPage(beforeID int64, limit int, model, group, statusClass string) (*LogPage, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT l.id,l.group_id,COALESCE(g.name,''),l.upstream_id,COALESCE(u.name,''),l.model,l.status,l.latency_ms,l.created_at,l.endpoint,l.key_name,l.retries
		FROM logs l LEFT JOIN upstreams u ON u.id=l.upstream_id LEFT JOIN groups g ON g.id=l.group_id WHERE 1=1`
	var args []any
	if beforeID > 0 {
		q += " AND l.id < ?"
		args = append(args, beforeID)
	}
	if model != "" {
		q += " AND l.model = ?"
		args = append(args, model)
	}
	if group != "" {
		q += " AND g.name = ?"
		args = append(args, group)
	}
	switch statusClass {
	case "ok":
		q += " AND l.status >= 200 AND l.status < 400"
	case "fail":
		q += " AND (l.status < 200 OR l.status >= 400)"
	}
	q += " ORDER BY l.id DESC LIMIT ?"
	args = append(args, limit+1) // 多取一条探测 hasMore
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &LogPage{Entries: []*LogEntry{}}
	for rows.Next() {
		e := &LogEntry{}
		if err := rows.Scan(&e.ID, &e.GroupID, &e.GroupName, &e.UpstreamID, &e.UpstreamName, &e.Model, &e.Status, &e.LatencyMs, &e.CreatedAt, &e.Endpoint, &e.KeyName, &e.Retries); err != nil {
			return nil, err
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Entries) > limit { // 取到了探测行 → 还有更多
		page.Entries = page.Entries[:limit]
		page.HasMore = true
	}
	if n := len(page.Entries); n > 0 {
		page.NextCursor = page.Entries[n-1].ID
	}
	return page, nil
}

// LogFilterOptions 日志里出现过的模型/分组名（去重排序），供前端筛选下拉用全量值。
type LogFilterOptions struct {
	Models []string `json:"models"`
	Groups []string `json:"groups"`
}

func (s *Store) LogFilterOptions() (*LogFilterOptions, error) {
	opt := &LogFilterOptions{Models: []string{}, Groups: []string{}}
	collect := func(q string) ([]string, error) {
		rows, err := s.db.Query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			if v != "" {
				out = append(out, v)
			}
		}
		return out, rows.Err()
	}
	var err error
	if opt.Models, err = collect(`SELECT DISTINCT model FROM logs WHERE model<>'' ORDER BY model`); err != nil {
		return nil, err
	}
	if opt.Groups, err = collect(`SELECT DISTINCT g.name FROM logs l JOIN groups g ON g.id=l.group_id ORDER BY g.name`); err != nil {
		return nil, err
	}
	if opt.Models == nil {
		opt.Models = []string{}
	}
	if opt.Groups == nil {
		opt.Groups = []string{}
	}
	return opt, nil
}

// RouteSample 一条历史路由样本，供启动时重建 (上游,模型) 的延迟/成功率 EWMA。
type RouteSample struct {
	UpstreamID int64
	Model      string
	OK         bool
	LatencyMs  int64
}

// RecentSamples 取最近 limit 条日志、按时间【正序】(旧→新)返回，供 EWMA 顺序回放。
// 只取真实转发记录用于重建选路预估；网络失败(status=0)也算样本(OK=false、延迟不计)。
func (s *Store) RecentSamples(limit int) ([]RouteSample, error) {
	if limit <= 0 {
		limit = 2000
	}
	// 先按 id 倒序取最近 limit 条，再正序回放（子查询取最近、外层翻正序）
	rows, err := s.db.Query(`SELECT upstream_id,model,status,latency_ms FROM
		(SELECT id,upstream_id,model,status,latency_ms FROM logs ORDER BY id DESC LIMIT ?) ORDER BY id ASC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteSample
	for rows.Next() {
		var s RouteSample
		var status int
		if err := rows.Scan(&s.UpstreamID, &s.Model, &status, &s.LatencyMs); err != nil {
			return nil, err
		}
		s.OK = status >= 200 && status < 400
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListLogs 倒序返回最近 limit 条日志（limit<=0 时默认 100）。
func (s *Store) ListLogs(limit int) ([]*LogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT l.id,l.group_id,COALESCE(g.name,''),l.upstream_id,COALESCE(u.name,''),l.model,l.status,l.latency_ms,l.created_at
		FROM logs l LEFT JOIN upstreams u ON u.id=l.upstream_id LEFT JOIN groups g ON g.id=l.group_id ORDER BY l.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LogEntry
	for rows.Next() {
		e := &LogEntry{}
		if err := rows.Scan(&e.ID, &e.GroupID, &e.GroupName, &e.UpstreamID, &e.UpstreamName, &e.Model, &e.Status, &e.LatencyMs, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneLogs 只保留最新 keep 条调用日志，删除更旧的，返回删除行数。
// keep<=0 视为关闭清理。利用主键自增有序：取第 keep 新的 id 当阈值，删比它更小的。
// 日志数 <= keep 时子查询取到的最小 id 即全表最小，id < 它删不到任何行。
func (s *Store) PruneLogs(keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := s.db.Exec(
		`DELETE FROM logs WHERE id < (SELECT MIN(id) FROM (SELECT id FROM logs ORDER BY id DESC LIMIT ?))`,
		keep)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- 运行时设置（页面可配，key-value）---

func (s *Store) GetSetting(key, def string) string {
	var v string
	if s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v) == nil && v != "" {
		return v
	}
	return def
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
