// Package store 持久化配置、监控和请求审计，并兼容 PostgreSQL 与测试用 SQLite。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mirainya/muxapi/database/migrations"
	"github.com/mirainya/muxapi/internal/upstream"
	_ "modernc.org/sqlite"
)

// dbAdapter 统一两种数据库的占位符和时间表达式差异。
type dbAdapter struct {
	*sql.DB
	postgres bool
}

type txAdapter struct {
	*sql.Tx
	postgres bool
}

// bindPostgres 只替换 SQL 字符串字面量之外的问号占位符。
func bindPostgres(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	arg := 1
	inString := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			b.WriteByte(ch)
			if inString && i+1 < len(query) && query[i+1] == '\'' {
				b.WriteByte(query[i+1])
				i++
				continue
			}
			inString = !inString
			continue
		}
		if ch == '?' && !inString {
			fmt.Fprintf(&b, "$%d", arg)
			arg++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func (d *dbAdapter) bind(query string) string {
	if d.postgres {
		return bindPostgres(query)
	}
	return query
}

func (d *dbAdapter) Exec(query string, args ...any) (sql.Result, error) {
	return d.DB.Exec(d.bind(query), args...)
}

func (d *dbAdapter) Query(query string, args ...any) (*sql.Rows, error) {
	return d.DB.Query(d.bind(query), args...)
}

func (d *dbAdapter) QueryRow(query string, args ...any) *sql.Row {
	return d.DB.QueryRow(d.bind(query), args...)
}

func (d *dbAdapter) Begin() (*txAdapter, error) {
	tx, err := d.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &txAdapter{Tx: tx, postgres: d.postgres}, nil
}

func (t *txAdapter) Exec(query string, args ...any) (sql.Result, error) {
	if t.postgres {
		query = bindPostgres(query)
	}
	return t.Tx.Exec(query, args...)
}

func (t *txAdapter) QueryRow(query string, args ...any) *sql.Row {
	if t.postgres {
		query = bindPostgres(query)
	}
	return t.Tx.QueryRow(query, args...)
}

const requestQueueSize = 4096

type requestWrite struct {
	record  *RequestRecord
	barrier chan struct{}
}

// Store 封装数据库访问；请求审计通过有界队列异步串行写入。
type Store struct {
	db           *dbAdapter
	requestQueue chan requestWrite
	requestDone  chan struct{}
	requestDrops atomic.Uint64
	closeOnce    sync.Once
}

func newStore(db *dbAdapter) *Store {
	s := &Store{
		db:           db,
		requestQueue: make(chan requestWrite, requestQueueSize),
		requestDone:  make(chan struct{}),
	}
	go s.runRequestWriter()
	return s
}

func (s *Store) timeValue(value time.Time) any {
	if s.db.postgres {
		return value
	}
	return value.Unix()
}

func (s *Store) unixExpr(column string) string {
	if s.db.postgres {
		return "CAST(EXTRACT(EPOCH FROM " + column + ") AS BIGINT)"
	}
	return column
}

func (s *Store) hourExpr(column string) string {
	if s.db.postgres {
		return "CAST(EXTRACT(EPOCH FROM date_trunc('hour', " + column + ")) AS BIGINT)"
	}
	return "(" + column + "/3600)*3600"
}

// Group 虚拟接入点：拥有自己的上游池(经中间表)和接入密钥。
type Group struct {
	ID                   int64       `json:"id"`
	Name                 string      `json:"name"`
	Description          string      `json:"description"`
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
	ChannelProbe bool   `json:"-"` // 兼容旧数据；运行时不再改变熔断粒度
}

// Open 根据连接串选择数据库；PostgreSQL 会在返回前执行嵌入式迁移。
func Open(databaseURL string) (*Store, error) {
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		db, err := sql.Open("pgx", databaseURL)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(10)
		db.SetConnMaxIdleTime(5 * time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, fmt.Errorf("connect PostgreSQL: %w", err)
		}
		if err := runPostgresMigrations(ctx, db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate PostgreSQL: %w", err)
		}
		return newStore(&dbAdapter{DB: db, postgres: true}), nil
	}
	if databaseURL == "" {
		return nil, errors.New("MUXAPI_DATABASE_URL is required")
	}
	return openSQLite(databaseURL)
}

// runPostgresMigrations 按文件名顺序执行尚未记录的迁移，每个文件独占事务。
func runPostgresMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		var applied bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO schema_migrations(version) VALUES($1)`, version)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func openSQLite(path string) (*Store, error) {
	// 并发可靠性 PRAGMA（modernc.org/sqlite 经 DSN 的 _pragma 参数下发到每条连接）：
	//   busy_timeout(5000) 写锁竞争时最多等 5s 再返回 SQLITE_BUSY，避免并发写静默丢日志/探测数据；
	//   journal_mode(WAL)  读写不互斥，提升并发；foreign_keys(1) 启用外键约束。
	// path 已带 query（如 :memory: 变体或自定义参数）时用 & 续接，否则用 ? 起始，避免覆盖。
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	dsn := path + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 仅保留给单元测试和离线迁移；:memory: 每条连接是独立数据库，
	// 固定单连接可避免异步审计写入拿到没有 schema 的新连接。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err = db.Exec(schema); err != nil {
		return nil, err
	}
	// 迁移：旧库 upstreams 补 proxy 列（已存在则忽略报错）
	db.Exec(`ALTER TABLE upstreams ADD COLUMN proxy TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE upstreams ADD COLUMN protocol TEXT NOT NULL DEFAULT 'passthrough'`)
	db.Exec(`ALTER TABLE upstreams ADD COLUMN source TEXT NOT NULL DEFAULT ''`)
	db.Exec(`INSERT OR IGNORE INTO tags(name,color,sort_order)
		SELECT DISTINCT TRIM(source),'gray',0 FROM upstreams WHERE TRIM(source)<>''`)
	db.Exec(`INSERT OR IGNORE INTO upstream_tags(upstream_id,tag_id,is_primary)
		SELECT u.id,t.id,1 FROM upstreams u JOIN tags t ON LOWER(t.name)=LOWER(TRIM(u.source)) WHERE TRIM(u.source)<>''`)
	// 迁移：保留旧 channel_probe 列，运行时已固定使用渠道级熔断。
	db.Exec(`ALTER TABLE upstreams ADD COLUMN channel_probe INTEGER NOT NULL DEFAULT 1`)
	// 迁移：旧库 upstreams 补 sort_order 列（拖拽排序权重，0=未排过按 id）
	db.Exec(`ALTER TABLE upstreams ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`)
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
	db.Exec(`ALTER TABLE logs ADD COLUMN error_text TEXT NOT NULL DEFAULT ''`)
	// Request audit detail columns. Errors are ignored because fresh schemas already contain them.
	for _, statement := range []string{
		`ALTER TABLE requests ADD COLUMN stream INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN request_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN response_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN stream_completed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN last_event TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN upstream_request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN error_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN error_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN selection_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN health_before TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN health_after TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN response_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN stream INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN stream_completed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN last_event TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN upstream_request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN error_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN error_source TEXT NOT NULL DEFAULT ''`,
	} {
		db.Exec(statement)
	}
	return newStore(&dbAdapter{DB: db}), nil
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
	source   TEXT NOT NULL DEFAULT '',
	base_url TEXT NOT NULL,
	api_key  TEXT NOT NULL,
	proxy    TEXT NOT NULL DEFAULT '',
	protocol TEXT NOT NULL DEFAULT 'passthrough',
	enabled  INTEGER NOT NULL DEFAULT 1,
	channel_probe INTEGER NOT NULL DEFAULT 1,
	sort_order INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS tags (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	color      TEXT NOT NULL DEFAULT 'gray',
	sort_order INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tags_name_ci ON tags(LOWER(name));
CREATE TABLE IF NOT EXISTS upstream_tags (
	upstream_id INTEGER NOT NULL,
	tag_id      INTEGER NOT NULL,
	is_primary INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (upstream_id, tag_id),
	FOREIGN KEY (upstream_id) REFERENCES upstreams(id) ON DELETE CASCADE,
	FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_upstream_primary_tag ON upstream_tags(upstream_id) WHERE is_primary=1;
CREATE INDEX IF NOT EXISTS idx_upstream_tags_tag ON upstream_tags(tag_id, upstream_id);
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
	retries     INTEGER NOT NULL DEFAULT 0,
	error_text  TEXT NOT NULL DEFAULT ''
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
CREATE TABLE IF NOT EXISTS requests (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id        TEXT NOT NULL UNIQUE,
	group_id          INTEGER NOT NULL DEFAULT 0,
	final_upstream_id INTEGER NOT NULL DEFAULT 0,
	model             TEXT NOT NULL DEFAULT '',
	endpoint          TEXT NOT NULL DEFAULT '',
	key_name          TEXT NOT NULL DEFAULT '',
	status            INTEGER NOT NULL DEFAULT 0,
	outcome           TEXT NOT NULL,
	ttft_ms           INTEGER NOT NULL DEFAULT 0,
	duration_ms       INTEGER NOT NULL DEFAULT 0,
	attempt_count     INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	completed_at      INTEGER NOT NULL,
	error_text        TEXT NOT NULL DEFAULT '',
	stream            INTEGER NOT NULL DEFAULT 0,
	request_bytes     INTEGER NOT NULL DEFAULT 0,
	response_bytes    INTEGER NOT NULL DEFAULT 0,
	input_tokens      INTEGER NOT NULL DEFAULT 0,
	output_tokens     INTEGER NOT NULL DEFAULT 0,
	cached_tokens     INTEGER NOT NULL DEFAULT 0,
	stream_completed  INTEGER NOT NULL DEFAULT 0,
	last_event        TEXT NOT NULL DEFAULT '',
	upstream_request_id TEXT NOT NULL DEFAULT '',
	error_kind        TEXT NOT NULL DEFAULT '',
	error_source      TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS request_attempts (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id   TEXT NOT NULL,
	attempt_no   INTEGER NOT NULL,
	upstream_id  INTEGER NOT NULL DEFAULT 0,
	status       INTEGER NOT NULL DEFAULT 0,
	outcome      TEXT NOT NULL,
	ttft_ms      INTEGER NOT NULL DEFAULT 0,
	duration_ms  INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	completed_at INTEGER NOT NULL,
	error_text   TEXT NOT NULL DEFAULT '',
	priority     INTEGER NOT NULL DEFAULT 0,
	selection_reason TEXT NOT NULL DEFAULT '',
	health_before TEXT NOT NULL DEFAULT '',
	health_after TEXT NOT NULL DEFAULT '',
	response_bytes INTEGER NOT NULL DEFAULT 0,
	stream       INTEGER NOT NULL DEFAULT 0,
	stream_completed INTEGER NOT NULL DEFAULT 0,
	last_event   TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	upstream_request_id TEXT NOT NULL DEFAULT '',
	error_kind   TEXT NOT NULL DEFAULT '',
	error_source TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (request_id) REFERENCES requests(request_id) ON DELETE CASCADE,
	UNIQUE (request_id, attempt_no)
);
CREATE INDEX IF NOT EXISTS idx_probe_mon_time ON probe_results(monitor_id, created_at);
CREATE INDEX IF NOT EXISTS idx_logs_group_time ON logs(group_id, created_at);
CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at);
CREATE INDEX IF NOT EXISTS idx_requests_group_time ON requests(group_id, created_at);
CREATE INDEX IF NOT EXISTS idx_requests_model_time ON requests(model, created_at);
CREATE INDEX IF NOT EXISTS idx_requests_outcome_time ON requests(outcome, created_at);
CREATE INDEX IF NOT EXISTS idx_requests_key_time ON requests(key_name, created_at);
CREATE INDEX IF NOT EXISTS idx_requests_error_time ON requests(error_kind, created_at);
CREATE INDEX IF NOT EXISTS idx_attempts_request ON request_attempts(request_id, attempt_no);
CREATE INDEX IF NOT EXISTS idx_attempts_upstream_time ON request_attempts(upstream_id, created_at);`

// --- 上游全局池 ---

func (s *Store) ListTags() ([]upstream.Tag, error) {
	rows, err := s.db.Query(`SELECT id,name,color FROM tags ORDER BY sort_order,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []upstream.Tag
	for rows.Next() {
		var tag upstream.Tag
		if err := rows.Scan(&tag.ID, &tag.Name, &tag.Color); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func (s *Store) CreateTag(name, color string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`INSERT INTO tags(name,color,sort_order)
		VALUES(?,?,(SELECT COALESCE(MAX(sort_order),0)+1 FROM tags)) RETURNING id`, name, color).Scan(&id)
	return id, err
}

func (s *Store) UpdateTag(id int64, name, color string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE tags SET name=?,color=? WHERE id=?`, name, color, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE upstreams SET source=? WHERE id IN
		(SELECT upstream_id FROM upstream_tags WHERE tag_id=? AND is_primary=TRUE)`, name, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteTag(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE upstreams SET source='' WHERE id IN
		(SELECT upstream_id FROM upstream_tags WHERE tag_id=? AND is_primary=TRUE)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tags WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) loadUpstreamTags(list []*upstream.Upstream) error {
	if len(list) == 0 {
		return nil
	}
	byID := make(map[int64]*upstream.Upstream, len(list))
	for _, item := range list {
		item.Tags = []upstream.Tag{}
		item.TagIDs = []int64{}
		byID[item.ID] = item
	}
	rows, err := s.db.Query(`SELECT ut.upstream_id,t.id,t.name,t.color,ut.is_primary
		FROM upstream_tags ut JOIN tags t ON t.id=ut.tag_id ORDER BY t.sort_order,t.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var upstreamID int64
		var tag upstream.Tag
		if err := rows.Scan(&upstreamID, &tag.ID, &tag.Name, &tag.Color, &tag.IsPrimary); err != nil {
			return err
		}
		item := byID[upstreamID]
		if item == nil {
			continue
		}
		item.Tags = append(item.Tags, tag)
		item.TagIDs = append(item.TagIDs, tag.ID)
		if tag.IsPrimary {
			item.PrimaryTagID = tag.ID
		}
	}
	return rows.Err()
}

func replaceUpstreamTags(tx *txAdapter, upstreamID, primaryTagID int64, tagIDs []int64) error {
	if _, err := tx.Exec(`DELETE FROM upstream_tags WHERE upstream_id=?`, upstreamID); err != nil {
		return err
	}
	ids := make(map[int64]bool, len(tagIDs)+1)
	for _, id := range tagIDs {
		if id > 0 {
			ids[id] = false
		}
	}
	if primaryTagID > 0 {
		ids[primaryTagID] = true
	}
	for id, primary := range ids {
		if _, err := tx.Exec(`INSERT INTO upstream_tags(upstream_id,tag_id,is_primary) VALUES(?,?,?)`, upstreamID, id, primary); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`UPDATE upstreams SET source=COALESCE((SELECT t.name FROM upstream_tags ut
		JOIN tags t ON t.id=ut.tag_id WHERE ut.upstream_id=? AND ut.is_primary=TRUE),'') WHERE id=?`, upstreamID, upstreamID)
	return err
}

// scanUps 扫描含组内视图 priority/weight 的行（JOIN 查询用）。
func scanUps(rows *sql.Rows) ([]*upstream.Upstream, error) {
	defer rows.Close()
	var list []*upstream.Upstream
	for rows.Next() {
		u := &upstream.Upstream{}
		if err := rows.Scan(&u.ID, &u.Name, &u.Source, &u.BaseURL, &u.APIKey, &u.Proxy, &u.Protocol, &u.Enabled, &u.Priority, &u.Weight, &u.ChannelProbe); err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

// ListEnabledByGroup 返回某分组下启用的上游，JOIN 中间表填充组内 priority/weight，
// 按组内优先级升序（调度用）。
func (s *Store) ListEnabledByGroup(groupID int64) ([]*upstream.Upstream, error) {
	rows, err := s.db.Query(`SELECT u.id,u.name,u.source,u.base_url,u.api_key,u.proxy,u.protocol,u.enabled,gu.priority,gu.weight,u.channel_probe
		FROM upstreams u JOIN group_upstreams gu ON gu.upstream_id=u.id
		WHERE gu.group_id=? AND u.enabled=TRUE AND gu.enabled=TRUE ORDER BY gu.priority ASC`, groupID)
	if err != nil {
		return nil, err
	}
	return scanUps(rows)
}

// List 返回全部上游(含停用)，priority/weight 置 0（全局池无组内语义），供探测与后台管理。
func (s *Store) List() ([]*upstream.Upstream, error) {
	rows, err := s.db.Query(`SELECT id,name,source,base_url,api_key,proxy,protocol,enabled,0,0,channel_probe FROM upstreams ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	list, err := scanUps(rows)
	if err != nil {
		return nil, err
	}
	if err := s.loadUpstreamTags(list); err != nil {
		return nil, err
	}
	return list, nil
}

// Get 按 id 取单个上游（含完整 api_key，供连通测试用）。
func (s *Store) Get(id int64) (*upstream.Upstream, error) {
	u := &upstream.Upstream{}
	err := s.db.QueryRow(`SELECT id,name,source,base_url,api_key,proxy,protocol,enabled,channel_probe FROM upstreams WHERE id=?`, id).
		Scan(&u.ID, &u.Name, &u.Source, &u.BaseURL, &u.APIKey, &u.Proxy, &u.Protocol, &u.Enabled, &u.ChannelProbe)
	if err == nil {
		err = s.loadUpstreamTags([]*upstream.Upstream{u})
	}
	return u, err
}

func (s *Store) Create(u *upstream.Upstream) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRow(`INSERT INTO upstreams(name,source,base_url,api_key,proxy,protocol,enabled,channel_probe,sort_order)
		VALUES(?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(sort_order),0)+1 FROM upstreams)) RETURNING id`,
		u.Name, u.Source, u.BaseURL, u.APIKey, u.Proxy, u.Protocol, u.Enabled, u.ChannelProbe).Scan(&u.ID); err != nil {
		return err
	}
	if u.TagsSet {
		if err := replaceUpstreamTags(tx, u.ID, u.PrimaryTagID, u.TagIDs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReorderUpstreams 按给定 id 顺序写入 sort_order 权重（从 1 起）。
func (s *Store) ReorderUpstreams(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE upstreams SET sort_order=? WHERE id=?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Update(u *upstream.Upstream) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if u.APIKey == "" { // 留空则不改凭证（对齐后台「留空则不修改」语义）
		_, err = tx.Exec(`UPDATE upstreams SET name=?,source=?,base_url=?,proxy=?,protocol=?,enabled=?,channel_probe=? WHERE id=?`,
			u.Name, u.Source, u.BaseURL, u.Proxy, u.Protocol, u.Enabled, u.ChannelProbe, u.ID)
	} else {
		_, err = tx.Exec(`UPDATE upstreams SET name=?,source=?,base_url=?,api_key=?,proxy=?,protocol=?,enabled=?,channel_probe=? WHERE id=?`,
			u.Name, u.Source, u.BaseURL, u.APIKey, u.Proxy, u.Protocol, u.Enabled, u.ChannelProbe, u.ID)
	}
	if err != nil {
		return err
	}
	if u.TagsSet {
		if err := replaceUpstreamTags(tx, u.ID, u.PrimaryTagID, u.TagIDs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type UpstreamBatchUpdate struct {
	Enabled      *bool
	PrimaryTagID *int64
	AddTagIDs    []int64
	RemoveTagIDs []int64
}

// BatchUpdateUpstreams updates management metadata without touching credentials or routing membership.
func (s *Store) BatchUpdateUpstreams(ids []int64, update UpstreamBatchUpdate) error {
	if len(ids) == 0 || (update.Enabled == nil && update.PrimaryTagID == nil && len(update.AddTagIDs) == 0 && len(update.RemoveTagIDs) == 0) {
		return errors.New("batch update requires ids and at least one field")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if id <= 0 {
			return errors.New("invalid upstream id")
		}
		if update.Enabled != nil {
			if _, err := tx.Exec(`UPDATE upstreams SET enabled=? WHERE id=?`, *update.Enabled, id); err != nil {
				return err
			}
		}
		for _, tagID := range update.AddTagIDs {
			if tagID <= 0 {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO upstream_tags(upstream_id,tag_id,is_primary) VALUES(?,?,FALSE)
				ON CONFLICT(upstream_id,tag_id) DO NOTHING`, id, tagID); err != nil {
				return err
			}
		}
		for _, tagID := range update.RemoveTagIDs {
			if _, err := tx.Exec(`DELETE FROM upstream_tags WHERE upstream_id=? AND tag_id=? AND is_primary=FALSE`, id, tagID); err != nil {
				return err
			}
		}
		if update.PrimaryTagID != nil {
			if _, err := tx.Exec(`UPDATE upstream_tags SET is_primary=FALSE WHERE upstream_id=?`, id); err != nil {
				return err
			}
			if *update.PrimaryTagID > 0 {
				if _, err := tx.Exec(`INSERT INTO upstream_tags(upstream_id,tag_id,is_primary) VALUES(?,?,TRUE)
					ON CONFLICT(upstream_id,tag_id) DO UPDATE SET is_primary=TRUE`, id, *update.PrimaryTagID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE upstreams SET source=COALESCE((SELECT t.name FROM upstream_tags ut
				JOIN tags t ON t.id=ut.tag_id WHERE ut.upstream_id=? AND ut.is_primary=TRUE),'') WHERE id=?`, id, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) Delete(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM group_upstreams WHERE upstream_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM monitors WHERE upstream_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM upstreams WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.requestQueue) })
	<-s.requestDone
	return s.db.Close()
}

// --- 监控项 ---

// scanMonitors 扫描含渠道视图(name/base_url/api_key)的 JOIN 行。
func scanMonitors(rows *sql.Rows) ([]*Monitor, error) {
	defer rows.Close()
	var ms []*Monitor
	for rows.Next() {
		m := &Monitor{}
		if err := rows.Scan(&m.ID, &m.UpstreamID, &m.Model, &m.Name, &m.Enabled,
			&m.Stream, &m.ProbeText, &m.MaxTokens, &m.IntervalSec, &m.Path,
			&m.UpstreamName, &m.BaseURL, &m.APIKey, &m.Proxy, &m.ChannelProbe); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}

const monitorJoin = `SELECT m.id,m.upstream_id,m.model,m.name,m.enabled,m.stream,m.probe_text,m.max_tokens,m.interval_sec,m.path,u.name,u.base_url,u.api_key,u.proxy,u.channel_probe
	FROM monitors m JOIN upstreams u ON u.id=m.upstream_id`

// ListMonitors 全部监控项；enabledOnly 时只返回启用且渠道也启用的（探测用）。
func (s *Store) ListMonitors(enabledOnly bool) ([]*Monitor, error) {
	q := monitorJoin
	if enabledOnly {
		q += ` WHERE m.enabled=TRUE AND u.enabled=TRUE`
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
	var id int64
	err := s.db.QueryRow(`INSERT INTO monitors(upstream_id,model,name,enabled,stream,probe_text,max_tokens,interval_sec,path)
		VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`,
		m.UpstreamID, m.Model, m.Name, m.Enabled, m.Stream, m.ProbeText, m.MaxTokens, m.IntervalSec, m.Path).Scan(&id)
	return id, err
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
	since := s.timeValue(time.Now().Add(-24 * time.Hour))
	rows, err := s.db.Query(`SELECT
		g.id,g.name,g.description,
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
	var id int64
	err := s.db.QueryRow(`INSERT INTO groups(name,description) VALUES(?,?) RETURNING id`, name, desc).Scan(&id)
	return id, err
}

func (s *Store) UpdateGroup(id int64, name, desc string) error {
	_, err := s.db.Exec(`UPDATE groups SET name=?, description=? WHERE id=?`, name, desc, id)
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
	UpstreamID   int64  `json:"upstream_id"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	Protocol     string `json:"protocol"`
	Enabled      bool   `json:"enabled"`       // 全局开关
	GroupEnabled bool   `json:"group_enabled"` // 组内开关
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
	ChannelProbe bool   `json:"channel_probe"` // 兼容旧数据
}

func (s *Store) ListGroupMembers(groupID int64) ([]*Member, error) {
	rows, err := s.db.Query(`SELECT u.id,u.name,u.base_url,u.protocol,u.enabled,gu.enabled,gu.priority,gu.weight,u.channel_probe
		FROM upstreams u JOIN group_upstreams gu ON gu.upstream_id=u.id
		WHERE gu.group_id=? ORDER BY gu.priority ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ms []*Member
	for rows.Next() {
		m := &Member{}
		if err := rows.Scan(&m.UpstreamID, &m.Name, &m.BaseURL, &m.Protocol, &m.Enabled, &m.GroupEnabled, &m.Priority, &m.Weight, &m.ChannelProbe); err != nil {
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

// --- 请求审计 ---

type RequestAttemptRecord struct {
	AttemptNo         int
	UpstreamID        int64
	Priority          int
	SelectionReason   string
	HealthBefore      string
	HealthAfter       string
	Status            int
	Outcome           string
	TTFTMs            int64
	DurationMs        int64
	ResponseBytes     int64
	Stream            bool
	StreamCompleted   bool
	LastEvent         string
	InputTokens       int64
	OutputTokens      int64
	CachedTokens      int64
	UpstreamRequestID string
	ErrorKind         string
	ErrorSource       string
	CreatedAt         time.Time
	CompletedAt       time.Time
	Error             string
}

type RequestRecord struct {
	RequestID         string
	GroupID           int64
	FinalUpstreamID   int64
	Model             string
	Endpoint          string
	KeyName           string
	Stream            bool
	RequestBytes      int64
	ResponseBytes     int64
	InputTokens       int64
	OutputTokens      int64
	CachedTokens      int64
	StreamCompleted   bool
	LastEvent         string
	UpstreamRequestID string
	ErrorKind         string
	ErrorSource       string
	Status            int
	Outcome           string
	TTFTMs            int64
	DurationMs        int64
	CreatedAt         time.Time
	CompletedAt       time.Time
	Error             string
	Attempts          []RequestAttemptRecord
}

type RequestAttemptEntry struct {
	ID                int64  `json:"id"`
	AttemptNo         int    `json:"attempt_no"`
	UpstreamID        int64  `json:"upstream_id"`
	UpstreamName      string `json:"upstream_name"`
	Priority          int    `json:"priority"`
	SelectionReason   string `json:"selection_reason"`
	HealthBefore      string `json:"health_before"`
	HealthAfter       string `json:"health_after"`
	Status            int    `json:"status"`
	Outcome           string `json:"outcome"`
	TTFTMs            int64  `json:"ttft_ms"`
	DurationMs        int64  `json:"duration_ms"`
	ResponseBytes     int64  `json:"response_bytes"`
	Stream            bool   `json:"stream"`
	StreamCompleted   bool   `json:"stream_completed"`
	LastEvent         string `json:"last_event"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CachedTokens      int64  `json:"cached_tokens"`
	UpstreamRequestID string `json:"upstream_request_id"`
	ErrorKind         string `json:"error_kind"`
	ErrorSource       string `json:"error_source"`
	CreatedAt         int64  `json:"created_at"`
	CompletedAt       int64  `json:"completed_at"`
	Error             string `json:"error"`
}

type RequestRouteStep struct {
	AttemptNo    int    `json:"attempt_no"`
	UpstreamID   int64  `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	Status       int    `json:"status"`
	Outcome      string `json:"outcome"`
	ErrorKind    string `json:"error_kind"`
}

type RequestEntry struct {
	ID                int64                  `json:"id"`
	RequestID         string                 `json:"request_id"`
	GroupID           int64                  `json:"group_id"`
	GroupName         string                 `json:"group_name"`
	FinalUpstreamID   int64                  `json:"final_upstream_id"`
	FinalUpstreamName string                 `json:"final_upstream_name"`
	Model             string                 `json:"model"`
	Endpoint          string                 `json:"endpoint"`
	KeyName           string                 `json:"key_name"`
	Stream            bool                   `json:"stream"`
	RequestBytes      int64                  `json:"request_bytes"`
	ResponseBytes     int64                  `json:"response_bytes"`
	InputTokens       int64                  `json:"input_tokens"`
	OutputTokens      int64                  `json:"output_tokens"`
	CachedTokens      int64                  `json:"cached_tokens"`
	StreamCompleted   bool                   `json:"stream_completed"`
	LastEvent         string                 `json:"last_event"`
	UpstreamRequestID string                 `json:"upstream_request_id"`
	ErrorKind         string                 `json:"error_kind"`
	ErrorSource       string                 `json:"error_source"`
	Status            int                    `json:"status"`
	Outcome           string                 `json:"outcome"`
	TTFTMs            int64                  `json:"ttft_ms"`
	DurationMs        int64                  `json:"duration_ms"`
	AttemptCount      int                    `json:"attempt_count"`
	CreatedAt         int64                  `json:"created_at"`
	CompletedAt       int64                  `json:"completed_at"`
	Error             string                 `json:"error"`
	Route             []RequestRouteStep     `json:"route,omitempty"`
	Attempts          []*RequestAttemptEntry `json:"attempts,omitempty"`
}

type RequestPage struct {
	Entries    []*RequestEntry `json:"entries"`
	HasMore    bool            `json:"has_more"`
	NextCursor int64           `json:"next_cursor"`
}

// EnqueueRequest 非阻塞提交审计记录；队列已满时返回 false 并累计丢弃数。
func (s *Store) EnqueueRequest(record RequestRecord) bool {
	select {
	case s.requestQueue <- requestWrite{record: &record}:
		return true
	default:
		dropped := s.requestDrops.Add(1)
		if dropped == 1 || dropped%100 == 0 {
			slog.Error("request audit queue full", "dropped", dropped)
		}
		return false
	}
}

// FlushRequests 等待调用前已入队的审计记录写完，常用于测试和关闭流程。
func (s *Store) FlushRequests(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case s.requestQueue <- requestWrite{barrier: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RequestDrops 返回因队列满或数据库写入失败而丢失的审计数。
func (s *Store) RequestDrops() uint64 { return s.requestDrops.Load() }

// runRequestWriter 单协程消费队列，保证屏障与先前审计记录的顺序。
func (s *Store) runRequestWriter() {
	defer close(s.requestDone)
	for item := range s.requestQueue {
		if item.barrier != nil {
			close(item.barrier)
			continue
		}
		if err := s.writeRequest(*item.record); err != nil {
			s.requestDrops.Add(1)
			slog.Error("write request audit failed", "request_id", item.record.RequestID, "err", err)
		}
	}
}

func (s *Store) writeRequest(record RequestRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO requests(
		request_id,group_id,final_upstream_id,model,endpoint,key_name,status,outcome,
		ttft_ms,duration_ms,attempt_count,created_at,completed_at,error_text,stream,
		request_bytes,response_bytes,input_tokens,output_tokens,cached_tokens,stream_completed,
		last_event,upstream_request_id,error_kind,error_source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		record.RequestID, record.GroupID, record.FinalUpstreamID, record.Model, record.Endpoint,
		record.KeyName, record.Status, record.Outcome, record.TTFTMs, record.DurationMs,
		len(record.Attempts), s.timeValue(record.CreatedAt), s.timeValue(record.CompletedAt), record.Error,
		record.Stream, record.RequestBytes, record.ResponseBytes, record.InputTokens, record.OutputTokens,
		record.CachedTokens, record.StreamCompleted, record.LastEvent, record.UpstreamRequestID,
		record.ErrorKind, record.ErrorSource)
	if err != nil {
		return err
	}
	for _, attempt := range record.Attempts {
		_, err = tx.Exec(`INSERT INTO request_attempts(
			request_id,attempt_no,upstream_id,status,outcome,ttft_ms,duration_ms,created_at,completed_at,error_text,
			priority,selection_reason,health_before,health_after,response_bytes,stream,stream_completed,last_event,
			input_tokens,output_tokens,cached_tokens,upstream_request_id,error_kind,error_source)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			record.RequestID, attempt.AttemptNo, attempt.UpstreamID, attempt.Status, attempt.Outcome,
			attempt.TTFTMs, attempt.DurationMs, s.timeValue(attempt.CreatedAt),
			s.timeValue(attempt.CompletedAt), attempt.Error, attempt.Priority, attempt.SelectionReason,
			attempt.HealthBefore, attempt.HealthAfter, attempt.ResponseBytes, attempt.Stream,
			attempt.StreamCompleted, attempt.LastEvent, attempt.InputTokens, attempt.OutputTokens,
			attempt.CachedTokens, attempt.UpstreamRequestID, attempt.ErrorKind, attempt.ErrorSource)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

type RequestFilter struct {
	BeforeID   int64
	Limit      int
	Offset     int
	Model      string
	Group      string
	Status     string
	KeyName    string
	Endpoint   string
	ErrorKind  string
	Query      string
	Stream     string
	UpstreamID int64
	Since      time.Time
	Until      time.Time
	Retried    bool
	SlowMs     int64
}

func (s *Store) requestWhere(filter RequestFilter, includeCursor bool) (string, []any) {
	var where strings.Builder
	where.WriteString(" WHERE 1=1")
	var args []any
	if includeCursor && filter.BeforeID > 0 {
		where.WriteString(" AND r.id < ?")
		args = append(args, filter.BeforeID)
	}
	if filter.Model != "" {
		where.WriteString(" AND r.model = ?")
		args = append(args, filter.Model)
	}
	if filter.Group != "" {
		where.WriteString(" AND g.name = ?")
		args = append(args, filter.Group)
	}
	if filter.KeyName != "" {
		where.WriteString(" AND r.key_name = ?")
		args = append(args, filter.KeyName)
	}
	if filter.Endpoint != "" {
		where.WriteString(" AND r.endpoint = ?")
		args = append(args, filter.Endpoint)
	}
	if filter.ErrorKind != "" {
		where.WriteString(" AND r.error_kind = ?")
		args = append(args, filter.ErrorKind)
	}
	if filter.Query != "" {
		where.WriteString(" AND LOWER(CAST(r.request_id AS TEXT)) LIKE ?")
		args = append(args, strings.ToLower(filter.Query)+"%")
	}
	if filter.UpstreamID > 0 {
		where.WriteString(" AND EXISTS (SELECT 1 FROM request_attempts af WHERE af.request_id=r.request_id AND af.upstream_id=?)")
		args = append(args, filter.UpstreamID)
	}
	if !filter.Since.IsZero() {
		where.WriteString(" AND r.created_at >= ?")
		args = append(args, s.timeValue(filter.Since))
	}
	if !filter.Until.IsZero() {
		where.WriteString(" AND r.created_at < ?")
		args = append(args, s.timeValue(filter.Until))
	}
	if filter.Retried {
		where.WriteString(" AND r.attempt_count > 1")
	}
	if filter.SlowMs > 0 {
		where.WriteString(" AND r.duration_ms >= ?")
		args = append(args, filter.SlowMs)
	}
	switch filter.Stream {
	case "stream":
		where.WriteString(" AND r.stream=TRUE")
	case "nonstream":
		where.WriteString(" AND r.stream=FALSE")
	}
	switch filter.Status {
	case "direct_success":
		where.WriteString(" AND r.outcome='success' AND r.attempt_count<=1")
	case "failover_success":
		where.WriteString(" AND r.outcome='success' AND r.attempt_count>1")
	case "failed":
		where.WriteString(" AND r.outcome IN ('failed','unavailable')")
	case "partial":
		where.WriteString(" AND r.outcome='partial'")
	case "canceled":
		where.WriteString(" AND r.outcome='canceled'")
	case "client_error":
		where.WriteString(" AND r.outcome='client_error'")
	case "ok":
		where.WriteString(" AND r.outcome='success'")
	case "fail":
		where.WriteString(" AND r.outcome<>'success'")
	}
	return where.String(), args
}

func (s *Store) requestSelect(where string) string {
	return fmt.Sprintf(`SELECT r.id,r.request_id,r.group_id,COALESCE(g.name,''),
		r.final_upstream_id,COALESCE(u.name,''),r.model,r.endpoint,r.key_name,r.status,r.outcome,
		r.ttft_ms,r.duration_ms,r.attempt_count,%s,%s,r.error_text,r.stream,r.request_bytes,
		r.response_bytes,r.input_tokens,r.output_tokens,r.cached_tokens,r.stream_completed,r.last_event,
		r.upstream_request_id,r.error_kind,r.error_source
		FROM requests r
		LEFT JOIN upstreams u ON u.id=r.final_upstream_id
		LEFT JOIN groups g ON g.id=r.group_id%s`,
		s.unixExpr("r.created_at"), s.unixExpr("r.completed_at"), where)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequestEntry(row rowScanner) (*RequestEntry, error) {
	e := &RequestEntry{}
	err := row.Scan(
		&e.ID, &e.RequestID, &e.GroupID, &e.GroupName, &e.FinalUpstreamID,
		&e.FinalUpstreamName, &e.Model, &e.Endpoint, &e.KeyName, &e.Status, &e.Outcome,
		&e.TTFTMs, &e.DurationMs, &e.AttemptCount, &e.CreatedAt, &e.CompletedAt, &e.Error,
		&e.Stream, &e.RequestBytes, &e.ResponseBytes, &e.InputTokens, &e.OutputTokens,
		&e.CachedTokens, &e.StreamCompleted, &e.LastEvent, &e.UpstreamRequestID,
		&e.ErrorKind, &e.ErrorSource,
	)
	return e, err
}

func (s *Store) ListRequestsPage(filter RequestFilter) (*RequestPage, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	where, args := s.requestWhere(filter, true)
	q := s.requestSelect(where) + " ORDER BY r.id DESC LIMIT ?"
	args = append(args, limit+1)
	if filter.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, filter.Offset)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &RequestPage{Entries: []*RequestEntry{}}
	for rows.Next() {
		e, err := scanRequestEntry(rows)
		if err != nil {
			return nil, err
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		page.HasMore = true
	}
	if err := s.loadRouteSummaries(page.Entries); err != nil {
		return nil, err
	}
	if n := len(page.Entries); n > 0 {
		page.NextCursor = page.Entries[n-1].ID
	}
	return page, nil
}

func (s *Store) loadRouteSummaries(entries []*RequestEntry) error {
	if len(entries) == 0 {
		return nil
	}
	placeholders := make([]string, len(entries))
	args := make([]any, len(entries))
	byRequest := make(map[string]*RequestEntry, len(entries))
	for i, entry := range entries {
		placeholders[i] = "?"
		args[i] = entry.RequestID
		byRequest[entry.RequestID] = entry
	}
	q := `SELECT CAST(a.request_id AS TEXT),a.attempt_no,a.upstream_id,COALESCE(u.name,''),
		a.status,a.outcome,a.error_kind FROM request_attempts a
		LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE CAST(a.request_id AS TEXT) IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY a.request_id,a.attempt_no`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var requestID string
		var step RequestRouteStep
		if err := rows.Scan(&requestID, &step.AttemptNo, &step.UpstreamID, &step.UpstreamName,
			&step.Status, &step.Outcome, &step.ErrorKind); err != nil {
			return err
		}
		if entry := byRequest[requestID]; entry != nil {
			entry.Route = append(entry.Route, step)
		}
	}
	return rows.Err()
}

func (s *Store) GetRequest(id int64) (*RequestEntry, error) {
	entry, err := scanRequestEntry(s.db.QueryRow(s.requestSelect(" WHERE r.id=?"), id))
	if err != nil {
		return nil, err
	}
	entry.Attempts, err = s.listRequestAttempts(entry.RequestID)
	return entry, err
}

func (s *Store) listRequestAttempts(requestID string) ([]*RequestAttemptEntry, error) {
	q := fmt.Sprintf(`SELECT a.id,a.attempt_no,a.upstream_id,COALESCE(u.name,''),a.status,a.outcome,
		a.ttft_ms,a.duration_ms,%s,%s,a.error_text,a.priority,a.selection_reason,a.health_before,
		a.health_after,a.response_bytes,a.stream,a.stream_completed,a.last_event,a.input_tokens,
		a.output_tokens,a.cached_tokens,a.upstream_request_id,a.error_kind,a.error_source
		FROM request_attempts a LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE a.request_id=? ORDER BY a.attempt_no`,
		s.unixExpr("a.created_at"), s.unixExpr("a.completed_at"))
	rows, err := s.db.Query(q, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RequestAttemptEntry{}
	for rows.Next() {
		e := &RequestAttemptEntry{}
		if err := rows.Scan(&e.ID, &e.AttemptNo, &e.UpstreamID, &e.UpstreamName, &e.Status,
			&e.Outcome, &e.TTFTMs, &e.DurationMs, &e.CreatedAt, &e.CompletedAt, &e.Error,
			&e.Priority, &e.SelectionReason, &e.HealthBefore, &e.HealthAfter, &e.ResponseBytes,
			&e.Stream, &e.StreamCompleted, &e.LastEvent, &e.InputTokens, &e.OutputTokens,
			&e.CachedTokens, &e.UpstreamRequestID, &e.ErrorKind, &e.ErrorSource); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type LogFilterOptions struct {
	Models     []string            `json:"models"`
	Groups     []string            `json:"groups"`
	Keys       []string            `json:"keys"`
	Endpoints  []string            `json:"endpoints"`
	ErrorKinds []string            `json:"error_kinds"`
	Upstreams  []LogUpstreamOption `json:"upstreams"`
}

type LogUpstreamOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *Store) LogFilterOptions() (*LogFilterOptions, error) {
	opt := &LogFilterOptions{
		Models: []string{}, Groups: []string{}, Keys: []string{}, Endpoints: []string{},
		ErrorKinds: []string{}, Upstreams: []LogUpstreamOption{},
	}
	collect := func(q string) ([]string, error) {
		rows, err := s.db.Query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return nil, err
			}
			if value != "" {
				out = append(out, value)
			}
		}
		return out, rows.Err()
	}
	var err error
	if opt.Models, err = collect(`SELECT DISTINCT model FROM requests WHERE model<>'' ORDER BY model`); err != nil {
		return nil, err
	}
	if opt.Groups, err = collect(`SELECT DISTINCT g.name FROM requests r JOIN groups g ON g.id=r.group_id ORDER BY g.name`); err != nil {
		return nil, err
	}
	if opt.Keys, err = collect(`SELECT DISTINCT key_name FROM requests WHERE key_name<>'' ORDER BY key_name`); err != nil {
		return nil, err
	}
	if opt.Endpoints, err = collect(`SELECT DISTINCT endpoint FROM requests WHERE endpoint<>'' ORDER BY endpoint`); err != nil {
		return nil, err
	}
	if opt.ErrorKinds, err = collect(`SELECT DISTINCT error_kind FROM requests WHERE error_kind<>'' ORDER BY error_kind`); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT DISTINCT a.upstream_id,COALESCE(u.name,'')
		FROM request_attempts a LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE a.upstream_id>0 ORDER BY a.upstream_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item LogUpstreamOption
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		if item.Name == "" {
			item.Name = fmt.Sprintf("#%d", item.ID)
		}
		opt.Upstreams = append(opt.Upstreams, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return opt, nil
}

type RequestStats struct {
	Total           int64   `json:"total"`
	DirectSuccess   int64   `json:"direct_success"`
	FailoverSuccess int64   `json:"failover_success"`
	Failed          int64   `json:"failed"`
	Partial         int64   `json:"partial"`
	Canceled        int64   `json:"canceled"`
	ClientError     int64   `json:"client_error"`
	Retried         int64   `json:"retried"`
	SuccessRate     float64 `json:"success_rate"`
	P50TTFTMs       int64   `json:"p50_ttft_ms"`
	P95TTFTMs       int64   `json:"p95_ttft_ms"`
	P95DurationMs   int64   `json:"p95_duration_ms"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
}

func (s *Store) RequestStats(filter RequestFilter) (*RequestStats, error) {
	where, args := s.requestWhere(filter, false)
	if s.db.postgres {
		stats := &RequestStats{}
		err := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN r.outcome='success' AND r.attempt_count<=1 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='success' AND r.attempt_count>1 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome NOT IN ('success','partial','canceled','client_error') THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='partial' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='canceled' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.outcome='client_error' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.attempt_count>1 THEN 1 ELSE 0 END),0),
			COALESCE(CAST(percentile_cont(0.50) WITHIN GROUP (ORDER BY r.ttft_ms)
				FILTER (WHERE r.ttft_ms>0) AS BIGINT),0),
			COALESCE(CAST(percentile_cont(0.95) WITHIN GROUP (ORDER BY r.ttft_ms)
				FILTER (WHERE r.ttft_ms>0) AS BIGINT),0),
			COALESCE(CAST(percentile_cont(0.95) WITHIN GROUP (ORDER BY r.duration_ms)
				FILTER (WHERE r.duration_ms>0) AS BIGINT),0),
			COALESCE(SUM(r.input_tokens),0),COALESCE(SUM(r.output_tokens),0),COALESCE(SUM(r.cached_tokens),0)
			FROM requests r LEFT JOIN groups g ON g.id=r.group_id`+where, args...).Scan(
			&stats.Total, &stats.DirectSuccess, &stats.FailoverSuccess, &stats.Failed,
			&stats.Partial, &stats.Canceled, &stats.ClientError, &stats.Retried,
			&stats.P50TTFTMs, &stats.P95TTFTMs, &stats.P95DurationMs,
			&stats.InputTokens, &stats.OutputTokens, &stats.CachedTokens,
		)
		if err != nil {
			return nil, err
		}
		if stats.Total > 0 {
			stats.SuccessRate = float64(stats.DirectSuccess+stats.FailoverSuccess) / float64(stats.Total)
		}
		return stats, nil
	}
	rows, err := s.db.Query(`SELECT r.outcome,r.attempt_count,r.ttft_ms,r.duration_ms,
		r.input_tokens,r.output_tokens,r.cached_tokens
		FROM requests r LEFT JOIN groups g ON g.id=r.group_id`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := &RequestStats{}
	var ttfts, durations []int64
	for rows.Next() {
		var outcome string
		var attempts int
		var ttft, duration, input, output, cached int64
		if err := rows.Scan(&outcome, &attempts, &ttft, &duration, &input, &output, &cached); err != nil {
			return nil, err
		}
		stats.Total++
		if attempts > 1 {
			stats.Retried++
		}
		switch outcome {
		case "success":
			if attempts > 1 {
				stats.FailoverSuccess++
			} else {
				stats.DirectSuccess++
			}
		case "partial":
			stats.Partial++
		case "canceled":
			stats.Canceled++
		case "client_error":
			stats.ClientError++
		default:
			stats.Failed++
		}
		if ttft > 0 {
			ttfts = append(ttfts, ttft)
		}
		if duration > 0 {
			durations = append(durations, duration)
		}
		stats.InputTokens += input
		stats.OutputTokens += output
		stats.CachedTokens += cached
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if stats.Total > 0 {
		stats.SuccessRate = float64(stats.DirectSuccess+stats.FailoverSuccess) / float64(stats.Total)
	}
	stats.P50TTFTMs = percentile(ttfts, 0.50)
	stats.P95TTFTMs = percentile(ttfts, 0.95)
	stats.P95DurationMs = percentile(durations, 0.95)
	return stats, nil
}

func percentile(values []int64, quantile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := int(float64(len(values)-1)*quantile + 0.5)
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

// RouteSample 一条历史路由样本，供启动时重建 (上游,模型) 的延迟/成功率 EWMA。
type RouteSample struct {
	UpstreamID int64
	Model      string
	OK         bool
	LatencyMs  int64
}

// RecentSamples 只回放会影响路由健康的成功或失败尝试。
func (s *Store) RecentSamples(limit int) ([]RouteSample, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := s.db.Query(`SELECT a.upstream_id,r.model,a.outcome,a.ttft_ms FROM
		(SELECT id,request_id,upstream_id,outcome,ttft_ms FROM request_attempts
		 WHERE upstream_id>0 AND outcome IN ('success','failed') ORDER BY id DESC LIMIT ?) a
		JOIN requests r ON r.request_id=a.request_id ORDER BY a.id ASC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteSample
	for rows.Next() {
		var s RouteSample
		var outcome string
		if err := rows.Scan(&s.UpstreamID, &s.Model, &outcome, &s.LatencyMs); err != nil {
			return nil, err
		}
		s.OK = outcome == "success"
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) ListRequests(limit int) ([]*RequestEntry, error) {
	page, err := s.ListRequestsPage(RequestFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	for _, entry := range page.Entries {
		entry.Attempts, err = s.listRequestAttempts(entry.RequestID)
		if err != nil {
			return nil, err
		}
	}
	return page.Entries, nil
}

// PruneRequests 每轮分批删除超过 keepDays 天的请求，尝试记录由外键级联删除。
func (s *Store) PruneRequests(keepDays, batch int) (int64, error) {
	if keepDays <= 0 || batch <= 0 {
		return 0, nil
	}
	cutoff := s.timeValue(time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour))
	res, err := s.db.Exec(`DELETE FROM requests WHERE id IN (
		SELECT id FROM requests WHERE created_at < ? ORDER BY created_at LIMIT ?
	)`, cutoff, batch)
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
