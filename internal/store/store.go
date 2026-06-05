package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
type Monitor struct {
	ID           int64  `json:"id"`
	UpstreamID   int64  `json:"upstream_id"`
	Model        string `json:"model"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
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
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream_id INTEGER NOT NULL,
	model       TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS logs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	group_id    INTEGER NOT NULL,
	upstream_id INTEGER NOT NULL,
	status      INTEGER NOT NULL,
	latency_ms  INTEGER NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

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
		WHERE gu.group_id=? AND u.enabled=1 ORDER BY gu.priority ASC`, groupID)
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
			&m.UpstreamName, &m.BaseURL, &m.APIKey, &m.Proxy); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}

const monitorJoin = `SELECT m.id,m.upstream_id,m.model,m.name,m.enabled,u.name,u.base_url,u.api_key,u.proxy
	FROM monitors m JOIN upstreams u ON u.id=m.upstream_id`

// ListMonitors 全部监控项；enabledOnly 时只返回启用且渠道也启用的（探测用）。
func (s *Store) ListMonitors(enabledOnly bool) ([]*Monitor, error) {
	q := monitorJoin
	if enabledOnly {
		q += ` WHERE m.enabled=1 AND u.enabled=1`
	}
	rows, err := s.db.Query(q + ` ORDER BY m.id`)
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
	res, err := s.db.Exec(`INSERT INTO monitors(upstream_id,model,name,enabled) VALUES(?,?,?,?)`,
		m.UpstreamID, m.Model, m.Name, m.Enabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateMonitor(m *Monitor) error {
	_, err := s.db.Exec(`UPDATE monitors SET upstream_id=?,model=?,name=?,enabled=? WHERE id=?`,
		m.UpstreamID, m.Model, m.Name, m.Enabled, m.ID)
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
		COUNT(DISTINCT CASE WHEN u.enabled=1 THEN u.id END),
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
	return gs, rows.Err()
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
type Member struct {
	UpstreamID int64  `json:"upstream_id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Enabled    bool   `json:"enabled"`
	Priority   int    `json:"priority"`
	Weight     int    `json:"weight"`
}

func (s *Store) ListGroupMembers(groupID int64) ([]*Member, error) {
	rows, err := s.db.Query(`SELECT u.id,u.name,u.base_url,u.enabled,gu.priority,gu.weight
		FROM upstreams u JOIN group_upstreams gu ON gu.upstream_id=u.id
		WHERE gu.group_id=? ORDER BY gu.priority ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ms []*Member
	for rows.Next() {
		m := &Member{}
		if err := rows.Scan(&m.UpstreamID, &m.Name, &m.BaseURL, &m.Enabled, &m.Priority, &m.Weight); err != nil {
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

// --- 接入 key ---
// GroupByKey 根据接入 key 找到其绑定的分组 id（路由核心），仅匹配启用的 key。
func (s *Store) GroupByKey(key string) (int64, bool) {
	var gid int64
	err := s.db.QueryRow(`SELECT group_id FROM access_keys WHERE key=? AND enabled=1`, key).Scan(&gid)
	return gid, err == nil
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

// LogEntry 一条转发日志（JOIN 出上游名供展示）。
type LogEntry struct {
	ID           int64  `json:"id"`
	GroupID      int64  `json:"group_id"`
	UpstreamID   int64  `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	Status       int    `json:"status"` // HTTP 状态码；网络失败为 0
	LatencyMs    int64  `json:"latency_ms"`
	CreatedAt    int64  `json:"created_at"`
}

// Log 记一条转发调用日志（异步友好：忽略写入错误，不阻塞转发）。
func (s *Store) Log(groupID, upstreamID int64, status int, latencyMs int64) {
	s.db.Exec(`INSERT INTO logs(group_id,upstream_id,status,latency_ms,created_at) VALUES(?,?,?,?,?)`,
		groupID, upstreamID, status, latencyMs, time.Now().Unix())
}

// ListLogs 倒序返回最近 limit 条日志（limit<=0 时默认 100）。
func (s *Store) ListLogs(limit int) ([]*LogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT l.id,l.group_id,l.upstream_id,COALESCE(u.name,''),l.status,l.latency_ms,l.created_at
		FROM logs l LEFT JOIN upstreams u ON u.id=l.upstream_id ORDER BY l.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LogEntry
	for rows.Next() {
		e := &LogEntry{}
		if err := rows.Scan(&e.ID, &e.GroupID, &e.UpstreamID, &e.UpstreamName, &e.Status, &e.LatencyMs, &e.CreatedAt); err != nil {
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
