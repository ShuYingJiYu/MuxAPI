// Package store 持久化配置、监控和请求审计，并兼容 PostgreSQL 与测试用 SQLite。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mirainya/muxapi/database/migrations"
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

// OpenOptions controls startup behavior for an existing database.
// ReadOnly skips PostgreSQL migrations; callers must ensure the schema exists.
type OpenOptions struct {
	ReadOnly bool
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

// bucketExpr returns a Unix bucket start expression shared by overview charts.
func (s *Store) bucketExpr(column string, seconds int64) string {
	if s.db.postgres {
		return fmt.Sprintf("CAST(FLOOR(EXTRACT(EPOCH FROM %s)/%d)*%d AS BIGINT)", column, seconds, seconds)
	}
	return fmt.Sprintf("(%s/%d)*%d", column, seconds, seconds)
}

// Open 根据连接串选择数据库；PostgreSQL 会在返回前执行嵌入式迁移。
func Open(databaseURL string) (*Store, error) {
	return OpenWithOptions(databaseURL, OpenOptions{})
}

// OpenWithOptions opens a store and optionally avoids startup schema writes.
func OpenWithOptions(databaseURL string, options OpenOptions) (*Store, error) {
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
		if !options.ReadOnly {
			if err := runPostgresMigrations(ctx, db); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate PostgreSQL: %w", err)
			}
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
	db.Exec(`ALTER TABLE upstreams ADD COLUMN billing_type TEXT NOT NULL DEFAULT 'none'`)
	db.Exec(`ALTER TABLE upstreams ADD COLUMN cache_mode TEXT NOT NULL DEFAULT 'auto'`)
	db.Exec(`ALTER TABLE upstreams ADD COLUMN source TEXT NOT NULL DEFAULT ''`)
	db.Exec(`INSERT OR IGNORE INTO tags(name,color,sort_order)
		SELECT DISTINCT TRIM(source),'gray',0 FROM upstreams WHERE TRIM(source)<>''`)
	db.Exec(`INSERT OR IGNORE INTO upstream_tags(upstream_id,tag_id,is_primary)
		SELECT u.id,t.id,1 FROM upstreams u JOIN tags t ON LOWER(t.name)=LOWER(TRIM(u.source)) WHERE TRIM(u.source)<>''`)
	// 迁移：保留旧 channel_probe 列，运行时已固定使用渠道级熔断。
	db.Exec(`ALTER TABLE upstreams ADD COLUMN channel_probe INTEGER NOT NULL DEFAULT 1`)
	// 迁移：旧库 upstreams 补 sort_order 列（拖拽排序权重，0=未排过按 id）
	db.Exec(`ALTER TABLE upstreams ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`)
	// 迁移：储值积分倍率（1:N 储值时填 N，用于归一化 max_multiplier 比对）
	db.Exec(`ALTER TABLE upstreams ADD COLUMN credit_ratio REAL NOT NULL DEFAULT 1`)
	// 迁移：旧库 groups 表补 sort_order 列（拖拽排序权重，0=未排序按 id）。
	db.Exec(`ALTER TABLE groups ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE groups ADD COLUMN max_multiplier REAL`)
	// Intelligent routing keeps request, billing, and probe history permanently
	// by default. Apply that upgrade once; a later administrator-selected
	// positive retention window must survive process restarts.
	var retentionMigration string
	_ = db.QueryRow(`SELECT value FROM settings WHERE key=?`, "intelligent_routing_retention_migrated").Scan(&retentionMigration)
	if retentionMigration != "1" {
		db.Exec(`INSERT INTO settings(key,value) VALUES('request_retention_days','0')
			ON CONFLICT(key) DO UPDATE SET value='0'`)
		db.Exec(`INSERT INTO settings(key,value) VALUES('billing_snapshot_retention_days','0')
			ON CONFLICT(key) DO UPDATE SET value='0'`)
		db.Exec(`INSERT INTO settings(key,value) VALUES('probe_retention_hours','0')
			ON CONFLICT(key) DO UPDATE SET value='0'`)
		db.Exec(`INSERT INTO settings(key,value) VALUES('intelligent_routing_retention_migrated','1')
			ON CONFLICT(key) DO UPDATE SET value='1'`)
	}
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
		`ALTER TABLE requests ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN stream_completed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN last_event TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN upstream_request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN error_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN error_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN client_ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN user_agent TEXT NOT NULL DEFAULT ''`,
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
		`ALTER TABLE request_attempts ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_attempts ADD COLUMN upstream_request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN error_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_attempts ADD COLUMN error_source TEXT NOT NULL DEFAULT ''`,
		// 快照当次尝试的渠道协议：费用比对靠它决定 cached_tokens 的口径，
		// 现查会用改后的协议解释历史用量。
		`ALTER TABLE request_attempts ADD COLUMN protocol TEXT NOT NULL DEFAULT ''`,
	} {
		db.Exec(statement)
	}
	return newStore(&dbAdapter{DB: db}), nil
}

const schema = `
CREATE TABLE IF NOT EXISTS groups (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	sort_order  INTEGER NOT NULL DEFAULT 0,
	max_multiplier REAL
);
CREATE TABLE IF NOT EXISTS upstreams (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	name     TEXT NOT NULL,
	source   TEXT NOT NULL DEFAULT '',
	base_url TEXT NOT NULL,
	api_key  TEXT NOT NULL,
	proxy    TEXT NOT NULL DEFAULT '',
	protocol TEXT NOT NULL DEFAULT 'passthrough',
	billing_type TEXT NOT NULL DEFAULT 'none',
	cache_mode TEXT NOT NULL DEFAULT 'auto',
	enabled  INTEGER NOT NULL DEFAULT 1,
	channel_probe INTEGER NOT NULL DEFAULT 1,
	sort_order INTEGER NOT NULL DEFAULT 0,
	credit_ratio REAL NOT NULL DEFAULT 1
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
CREATE TABLE IF NOT EXISTS upstream_billing_status (
	upstream_id INTEGER PRIMARY KEY,
	currency TEXT NOT NULL DEFAULT 'USD',
	remaining REAL,
	unlimited INTEGER NOT NULL DEFAULT 0,
	billing_group TEXT NOT NULL DEFAULT '',
	group_multiplier REAL,
	effective_multiplier REAL,
	reported_list_cost REAL,
	reported_actual_cost REAL,
	status TEXT NOT NULL DEFAULT 'pending',
	error_text TEXT NOT NULL DEFAULT '',
	observed_at INTEGER,
	last_success_at INTEGER,
	refreshed_at INTEGER NOT NULL,
	FOREIGN KEY (upstream_id) REFERENCES upstreams(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS upstream_billing_snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream_id INTEGER NOT NULL,
	currency TEXT NOT NULL DEFAULT 'USD',
	remaining REAL,
	unlimited INTEGER NOT NULL DEFAULT 0,
	billing_group TEXT NOT NULL DEFAULT '',
	group_multiplier REAL,
	effective_multiplier REAL,
	reported_list_cost REAL,
	reported_actual_cost REAL,
	observed_at INTEGER NOT NULL,
	FOREIGN KEY (upstream_id) REFERENCES upstreams(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS model_pricing (
	model TEXT PRIMARY KEY,
	input_cost_per_token REAL,
	output_cost_per_token REAL,
	cache_read_input_token_cost REAL,
	cache_creation_input_token_cost REAL
);
CREATE TABLE IF NOT EXISTS pricing_catalog_status (
	id INTEGER PRIMARY KEY CHECK (id=1),
	source TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	model_count INTEGER NOT NULL DEFAULT 0,
	last_checked_at INTEGER,
	last_success_at INTEGER,
	error_text TEXT NOT NULL DEFAULT ''
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
	client_ip         TEXT NOT NULL DEFAULT '',
	user_agent        TEXT NOT NULL DEFAULT '',
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
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
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
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	upstream_request_id TEXT NOT NULL DEFAULT '',
	error_kind   TEXT NOT NULL DEFAULT '',
	error_source TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (request_id) REFERENCES requests(request_id) ON DELETE CASCADE,
	UNIQUE (request_id, attempt_no)
);
CREATE TABLE IF NOT EXISTS route_decisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL UNIQUE,
	group_id INTEGER NOT NULL DEFAULT 0,
	model TEXT NOT NULL DEFAULT '',
	protocol TEXT NOT NULL DEFAULT '',
	endpoint TEXT NOT NULL DEFAULT '',
	session_key TEXT NOT NULL DEFAULT '',
	prefix_hash TEXT NOT NULL DEFAULT '',
	cache_key TEXT NOT NULL DEFAULT '',
	strategy TEXT NOT NULL DEFAULT 'cost',
	reason TEXT NOT NULL DEFAULT '',
	selected_upstream_id INTEGER NOT NULL DEFAULT 0,
	candidate_count INTEGER NOT NULL DEFAULT 0,
	forecast_window_seconds INTEGER NOT NULL DEFAULT 0,
	forecast_requests REAL NOT NULL DEFAULT 0,
	estimated_input_tokens INTEGER NOT NULL DEFAULT 0,
	reusable_prefix_tokens INTEGER NOT NULL DEFAULT 0,
	estimated_output_tokens INTEGER NOT NULL DEFAULT 0,
	selected_cost REAL,
	no_cache_cost REAL,
	cache_cost REAL,
	estimated_savings REAL,
	confidence REAL NOT NULL DEFAULT 0,
	cache_selected INTEGER NOT NULL DEFAULT 0,
	exploration INTEGER NOT NULL DEFAULT 0,
	actual_cost REAL,
	actual_input_tokens INTEGER,
	actual_output_tokens INTEGER,
	actual_cached_tokens INTEGER,
	actual_cache_creation_tokens INTEGER,
	actual_outcome TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	completed_at INTEGER
);
CREATE TABLE IF NOT EXISTS route_decision_candidates (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	decision_id INTEGER NOT NULL,
	upstream_id INTEGER NOT NULL DEFAULT 0,
	api_key_hash TEXT NOT NULL DEFAULT '',
	upstream_name TEXT NOT NULL DEFAULT '',
	protocol TEXT NOT NULL DEFAULT '',
	priority INTEGER NOT NULL DEFAULT 0,
	eligible INTEGER NOT NULL DEFAULT 1,
	selected INTEGER NOT NULL DEFAULT 0,
	rejection_reason TEXT NOT NULL DEFAULT '',
	pricing_source TEXT NOT NULL DEFAULT '',
	pricing_confidence REAL NOT NULL DEFAULT 0,
	cache_supported INTEGER NOT NULL DEFAULT 0,
	cache_existing INTEGER NOT NULL DEFAULT 0,
	cache_selected INTEGER NOT NULL DEFAULT 0,
	cache_hit_rate REAL NOT NULL DEFAULT 0,
	forecast_total_cost REAL,
	forecast_no_cache_cost REAL,
	forecast_cache_cost REAL,
	estimated_savings REAL,
	break_even_requests REAL,
	expected_hits REAL NOT NULL DEFAULT 0,
	expected_misses REAL NOT NULL DEFAULT 0,
	expected_creates REAL NOT NULL DEFAULT 0,
	estimated_ttft_ms REAL NOT NULL DEFAULT 0,
	estimated_duration_ms REAL NOT NULL DEFAULT 0,
	success_rate REAL NOT NULL DEFAULT 0,
	details_json TEXT NOT NULL DEFAULT '{}',
	FOREIGN KEY (decision_id) REFERENCES route_decisions(id) ON DELETE CASCADE,
	UNIQUE (decision_id, upstream_id)
);
CREATE TABLE IF NOT EXISTS routing_observations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL,
	attempt_no INTEGER NOT NULL DEFAULT 1,
	group_id INTEGER NOT NULL DEFAULT 0,
	upstream_id INTEGER NOT NULL DEFAULT 0,
	api_key_hash TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	session_key TEXT NOT NULL DEFAULT '',
	prefix_hash TEXT NOT NULL DEFAULT '',
	cache_key TEXT NOT NULL DEFAULT '',
	prefix_tokens INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	ttft_ms INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	success INTEGER NOT NULL DEFAULT 0,
	cache_eligible INTEGER NOT NULL DEFAULT 0,
	cache_hit INTEGER NOT NULL DEFAULT 0,
	cache_created INTEGER NOT NULL DEFAULT 0,
	cache_expires_at INTEGER,
	observed_at INTEGER NOT NULL,
	UNIQUE (request_id, attempt_no)
);
CREATE TABLE IF NOT EXISTS upstream_prefix_cache_stats (
	api_key_hash TEXT NOT NULL,
	upstream_id INTEGER NOT NULL,
	model TEXT NOT NULL,
	prefix_hash TEXT NOT NULL,
	session_key TEXT NOT NULL DEFAULT '',
	cache_key TEXT NOT NULL DEFAULT '',
	prefix_tokens INTEGER NOT NULL DEFAULT 0,
	observations INTEGER NOT NULL DEFAULT 0,
	hit_count INTEGER NOT NULL DEFAULT 0,
	miss_count INTEGER NOT NULL DEFAULT 0,
	create_count INTEGER NOT NULL DEFAULT 0,
	last_hit_at INTEGER,
	last_miss_at INTEGER,
	last_created_at INTEGER,
	expires_at INTEGER,
	first_seen_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	PRIMARY KEY (api_key_hash, upstream_id, model, prefix_hash)
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
CREATE INDEX IF NOT EXISTS idx_attempts_upstream_time ON request_attempts(upstream_id, created_at);
CREATE INDEX IF NOT EXISTS idx_attempts_upstream_completed ON request_attempts(upstream_id, completed_at);
CREATE INDEX IF NOT EXISTS idx_upstream_billing_snapshots_time ON upstream_billing_snapshots(upstream_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_route_decisions_created ON route_decisions(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_route_decisions_session_model ON route_decisions(session_key, model, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_route_decisions_prefix_model ON route_decisions(prefix_hash, model, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_route_candidates_upstream ON route_decision_candidates(upstream_id, decision_id DESC);
CREATE INDEX IF NOT EXISTS idx_routing_observations_time ON routing_observations(observed_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_routing_observations_session_model ON routing_observations(session_key, model, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_observations_prefix_model ON routing_observations(prefix_hash, model, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_observations_upstream_model ON routing_observations(upstream_id, model, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_upstream_prefix_cache_expiry ON upstream_prefix_cache_stats(api_key_hash, upstream_id, model, expires_at);`
