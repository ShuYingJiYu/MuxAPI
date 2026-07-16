package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type groupRow struct {
	id          int64
	name        string
	description string
}

type upstreamRow struct {
	id                                     int64
	name, baseURL, apiKey, proxy, protocol string
	enabled, channelProbe                  bool
	sortOrder                              int
}

type memberRow struct {
	groupID, upstreamID int64
	priority, weight    int
	enabled             bool
}

type keyRow struct {
	id      int64
	name    string
	key     string
	groupID int64
	enabled bool
}

type monitorRow struct {
	id, upstreamID                         int64
	model, name, probeText, path           string
	enabled, stream                        bool
	maxTokens, intervalSeconds, sortWeight int
}

type settingRow struct{ key, value string }

func main() {
	sourcePath := flag.String("source", "muxapi.db", "source SQLite database")
	flag.Parse()
	targetURL := os.Getenv("MUXAPI_DATABASE_URL")
	if targetURL == "" {
		log.Fatal("MUXAPI_DATABASE_URL is required")
	}

	source, err := sql.Open("sqlite", *sourcePath+"?_pragma=query_only(1)")
	if err != nil {
		log.Fatal(err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)

	groups := loadGroups(source)
	upstreams := loadUpstreams(source)
	members := loadMembers(source)
	keys := loadKeys(source)
	monitors := loadMonitors(source)
	settings := loadSettings(source)

	target, err := sql.Open("pgx", targetURL)
	if err != nil {
		log.Fatal(err)
	}
	defer target.Close()
	if err := target.Ping(); err != nil {
		log.Fatalf("connect target: %v", err)
	}
	tx, err := target.Begin()
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()

	for _, row := range groups {
		mustExec(tx, `INSERT INTO groups(id,name,description) VALUES($1,$2,$3)
			ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,description=EXCLUDED.description`,
			row.id, row.name, row.description)
	}
	for _, row := range upstreams {
		mustExec(tx, `INSERT INTO upstreams(id,name,base_url,api_key,proxy,protocol,enabled,channel_probe,sort_order)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(id) DO UPDATE SET
			name=EXCLUDED.name,base_url=EXCLUDED.base_url,api_key=EXCLUDED.api_key,
			proxy=EXCLUDED.proxy,protocol=EXCLUDED.protocol,enabled=EXCLUDED.enabled,channel_probe=EXCLUDED.channel_probe,
			sort_order=EXCLUDED.sort_order`, row.id, row.name, row.baseURL, row.apiKey, row.proxy,
			row.protocol, row.enabled, row.channelProbe, row.sortOrder)
	}
	for _, row := range members {
		mustExec(tx, `INSERT INTO group_upstreams(group_id,upstream_id,priority,weight,enabled)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT(group_id,upstream_id) DO UPDATE SET
			priority=EXCLUDED.priority,weight=EXCLUDED.weight,enabled=EXCLUDED.enabled`,
			row.groupID, row.upstreamID, row.priority, row.weight, row.enabled)
	}
	for _, row := range keys {
		mustExec(tx, `INSERT INTO access_keys(id,name,key,group_id,enabled) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,key=EXCLUDED.key,
			group_id=EXCLUDED.group_id,enabled=EXCLUDED.enabled`,
			row.id, row.name, row.key, row.groupID, row.enabled)
	}
	for _, row := range monitors {
		mustExec(tx, `INSERT INTO monitors(id,upstream_id,model,name,enabled,stream,probe_text,
			max_tokens,interval_sec,path,sort) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT(id) DO UPDATE SET upstream_id=EXCLUDED.upstream_id,model=EXCLUDED.model,
			name=EXCLUDED.name,enabled=EXCLUDED.enabled,stream=EXCLUDED.stream,
			probe_text=EXCLUDED.probe_text,max_tokens=EXCLUDED.max_tokens,
			interval_sec=EXCLUDED.interval_sec,path=EXCLUDED.path,sort=EXCLUDED.sort`,
			row.id, row.upstreamID, row.model, row.name, row.enabled, row.stream,
			row.probeText, row.maxTokens, row.intervalSeconds, row.path, row.sortWeight)
	}
	for _, row := range settings {
		mustExec(tx, `INSERT INTO settings(key,value) VALUES($1,$2)
			ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, row.key, row.value)
	}
	mustExec(tx, `INSERT INTO settings(key,value) VALUES('request_retention_days','7')
		ON CONFLICT(key) DO UPDATE SET value='7'`)
	for _, table := range []string{"groups", "upstreams", "access_keys", "monitors"} {
		mustExec(tx, fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%s','id'),
			COALESCE(MAX(id),1),COUNT(*)>0) FROM %s`, table, table))
	}
	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("groups=%d upstreams=%d members=%d keys=%d monitors=%d settings=%d\n",
		len(groups), len(upstreams), len(members), len(keys), len(monitors), len(settings)+1)
}

func mustExec(tx *sql.Tx, query string, args ...any) {
	if _, err := tx.Exec(query, args...); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
}

func loadGroups(db *sql.DB) []groupRow {
	rows := mustQuery(db, `SELECT id,name,description FROM groups ORDER BY id`)
	defer rows.Close()
	var out []groupRow
	for rows.Next() {
		var row groupRow
		mustScan(rows, &row.id, &row.name, &row.description)
		out = append(out, row)
	}
	mustRows(rows)
	return out
}

func loadUpstreams(db *sql.DB) []upstreamRow {
	protocolColumn := `'passthrough'`
	if sqliteColumnExists(db, "upstreams", "protocol") {
		protocolColumn = `COALESCE(protocol,'passthrough')`
	}
	rows := mustQuery(db, fmt.Sprintf(`SELECT id,name,base_url,api_key,proxy,%s,enabled,channel_probe,sort_order FROM upstreams ORDER BY id`, protocolColumn))
	defer rows.Close()
	var out []upstreamRow
	for rows.Next() {
		var row upstreamRow
		mustScan(rows, &row.id, &row.name, &row.baseURL, &row.apiKey, &row.proxy, &row.protocol, &row.enabled, &row.channelProbe, &row.sortOrder)
		out = append(out, row)
	}
	mustRows(rows)
	return out
}

func sqliteColumnExists(db *sql.DB, table, column string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey) == nil && name == column {
			return true
		}
	}
	return false
}

func loadMembers(db *sql.DB) []memberRow {
	rows := mustQuery(db, `SELECT group_id,upstream_id,priority,weight,enabled FROM group_upstreams ORDER BY group_id,upstream_id`)
	defer rows.Close()
	var out []memberRow
	for rows.Next() {
		var row memberRow
		mustScan(rows, &row.groupID, &row.upstreamID, &row.priority, &row.weight, &row.enabled)
		out = append(out, row)
	}
	mustRows(rows)
	return out
}

func loadKeys(db *sql.DB) []keyRow {
	rows := mustQuery(db, `SELECT id,name,key,group_id,enabled FROM access_keys ORDER BY id`)
	defer rows.Close()
	var out []keyRow
	for rows.Next() {
		var row keyRow
		mustScan(rows, &row.id, &row.name, &row.key, &row.groupID, &row.enabled)
		out = append(out, row)
	}
	mustRows(rows)
	return out
}

func loadMonitors(db *sql.DB) []monitorRow {
	rows := mustQuery(db, `SELECT id,upstream_id,model,name,enabled,stream,probe_text,
		max_tokens,interval_sec,path,sort FROM monitors ORDER BY id`)
	defer rows.Close()
	var out []monitorRow
	for rows.Next() {
		var row monitorRow
		mustScan(rows, &row.id, &row.upstreamID, &row.model, &row.name, &row.enabled, &row.stream,
			&row.probeText, &row.maxTokens, &row.intervalSeconds, &row.path, &row.sortWeight)
		out = append(out, row)
	}
	mustRows(rows)
	return out
}

func loadSettings(db *sql.DB) []settingRow {
	rows := mustQuery(db, `SELECT key,value FROM settings WHERE key <> 'log_retention' ORDER BY key`)
	defer rows.Close()
	var out []settingRow
	for rows.Next() {
		var row settingRow
		mustScan(rows, &row.key, &row.value)
		out = append(out, row)
	}
	mustRows(rows)
	return out
}

func mustQuery(db *sql.DB, query string) *sql.Rows {
	rows, err := db.Query(query)
	if err != nil {
		log.Fatal(err)
	}
	return rows
}

func mustScan(rows *sql.Rows, dest ...any) {
	if err := rows.Scan(dest...); err != nil {
		log.Fatal(err)
	}
}

func mustRows(rows *sql.Rows) {
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
}
