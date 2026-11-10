package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/tokenusage"
)

// Each connection receives its own search_path from the DSN. A session-local
// SET would leave other connections in Open's pool pointing at the caller's data.
func postgresRouteDecisionDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MUXAPI_TEST_PG")
	if dsn == "" {
		t.Skip("MUXAPI_TEST_PG is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatal("MUXAPI_TEST_PG must be a postgres:// or postgresql:// URL")
	}
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "muxapi_route_test_" + hex.EncodeToString(suffix[:])
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close schema administration connection: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Do not use IF NOT EXISTS: cleanup is registered only after this test
	// successfully creates a new schema, never for a pre-existing schema.
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create isolated PostgreSQL schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("remove test-owned PostgreSQL schema: %v", err)
		}
	})
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	return u.String()
}

func TestPostgresRouteDecisions(t *testing.T) {
	for _, shape := range []struct {
		name       string
		legacyText bool
	}{
		{name: "uuid_uuid"},
		{name: "uuid_text", legacyText: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			const migration = "20261110_120000_add_attempt_request_text_index"
			dsn := postgresRouteDecisionDSN(t)
			s, err := Open(dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if s != nil {
					if err := s.Close(); err != nil {
						t.Errorf("close PostgreSQL store: %v", err)
					}
				}
			})
			// Model the historical GORM-created routing table while retaining the
			// complete application schema and all real migrations from Open.
			if shape.legacyText {
				if _, err := s.exec(`ALTER TABLE route_decisions ALTER COLUMN request_id TYPE TEXT USING request_id::text`); err != nil {
					t.Fatal(err)
				}
				// Restore the pre-index migration state inside this test's schema
				// and reopen, so the real migration also runs on the legacy shape.
				if _, err := s.exec(`DROP INDEX idx_attempts_request_text`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.exec(`DELETE FROM schema_migrations WHERE version=?`, migration); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(dsn)
				if err != nil {
					t.Fatalf("migrate legacy routing schema: %v", err)
				}
			}
			for table, want := range map[string]string{
				"request_attempts": "uuid",
				"route_decisions":  "uuid",
			} {
				if table == "route_decisions" && shape.legacyText {
					want = "text"
				}
				var got string
				if err := s.queryRow(`SELECT data_type FROM information_schema.columns
					WHERE table_schema=current_schema() AND table_name=? AND column_name='request_id'`, table).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("%s.request_id type = %q, want %q", table, got, want)
				}
			}

			var upstreamID int64
			if err := s.queryRow(`INSERT INTO upstreams(name,base_url,api_key,protocol)
				VALUES('legacy-fallback','http://localhost','test','claude') RETURNING id`).Scan(&upstreamID); err != nil {
				t.Fatal(err)
			}
			report := tokenusage.Parse(map[string]any{"input_tokens": int64(777), "cached_tokens": int64(600)}, "responses")
			report.Complete = true
			snapshot := tokenusage.New(tokenusage.Estimate{Tokens: 777, Model: "snapshot", Protocol: "responses", Source: "local_tiktoken", Covered: true}, report)
			for i, tc := range []struct {
				name         string
				protocols    []string
				wantProtocol string
				wantPrompt   int64
				unknown      bool
				snapshot     bool
				malformed    bool
			}{
				{name: "latest_responses", protocols: []string{"responses", "claude"}, wantProtocol: "responses", wantPrompt: 1000},
				{name: "latest_claude", protocols: []string{"claude", "responses"}, wantProtocol: "claude", wantPrompt: 1900},
				{name: "latest_unknown", protocols: []string{"", "responses"}, unknown: true},
				{name: "no_attempt", unknown: true},
				{name: "snapshot_precedence", protocols: []string{"claude", "responses"}, wantProtocol: "responses", wantPrompt: 777, snapshot: true},
				{name: "malformed_legacy_id", unknown: true, malformed: true},
			} {
				if tc.malformed && !shape.legacyText {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					requestID := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
					if tc.malformed {
						requestID = "legacy-request-without-a-uuid"
					}
					now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
					if len(tc.protocols) > 0 {
						if _, err := s.exec(`INSERT INTO requests(request_id,outcome,created_at,completed_at) VALUES(?,'success',?,?)`, requestID, now, now); err != nil {
							t.Fatal(err)
						}
					}
					for j, protocol := range tc.protocols {
						// Highest attempt number is inserted first and has the oldest
						// timestamp, so neither row ID nor timestamp may choose it.
						when := now.Add(time.Duration(j) * time.Second)
						if _, err := s.exec(`INSERT INTO request_attempts(request_id,attempt_no,protocol,outcome,created_at,completed_at)
							VALUES(?,?,?,'success',?,?)`, requestID, 7-j*6, protocol, when, when); err != nil {
							t.Fatal(err)
						}
					}
					id, err := s.SaveRouteDecision(RouteDecisionRecord{RequestID: requestID, Model: tc.name, Protocol: "claude", SelectedUpstreamID: upstreamID, EstimatedInputTokens: 1000, CreatedAt: now})
					if err != nil {
						t.Fatal(err)
					}
					input, cached := int64(1000), int64(900)
					outcome := RouteDecisionOutcome{ActualInputTokens: &input, ActualCachedTokens: &cached, CompletedAt: now}
					if tc.snapshot {
						outcome.TokenAccounting = snapshot
					}
					if err := s.CompleteRouteDecision(requestID, outcome); err != nil {
						t.Fatal(err)
					}
					assertEntry := func(entry *RouteDecisionEntry) {
						t.Helper()
						if entry == nil || entry.ID != id || entry.RequestID != requestID {
							t.Fatalf("wrong route decision: %+v", entry)
						}
						if entry.TokenAccounting == nil || entry.TokenAccounting.Report.Protocol != tc.wantProtocol {
							t.Fatalf("accounting = %+v, want report protocol %q", entry.TokenAccounting, tc.wantProtocol)
						}
						if tc.unknown {
							if entry.ActualPromptTokens != nil || entry.TokenAccounting.Report.InputTotal != nil {
								t.Fatal("unknown attempt protocol invented a prompt total")
							}
						} else if entry.ActualPromptTokens == nil || *entry.ActualPromptTokens != tc.wantPrompt {
							t.Fatalf("actual prompt tokens = %v, want %d; accounting = %+v", entry.ActualPromptTokens, tc.wantPrompt, entry.TokenAccounting)
						}
						if tc.snapshot {
							if tokenusage.Encode(entry.TokenAccounting) != tokenusage.Encode(snapshot) {
								t.Fatal("persisted accounting snapshot was replaced by legacy columns or attempt protocol")
							}
						} else if entry.TokenAccounting.Ratio != nil || entry.TokenAccounting.Status != "legacy" {
							t.Fatalf("legacy accounting invented a baseline: %+v", entry.TokenAccounting)
						}
					}
					entries, err := s.ListRouteDecisions(RouteDecisionFilter{Model: tc.name})
					if err != nil || len(entries) != 1 {
						t.Fatalf("ListRouteDecisions: entries=%d err=%v", len(entries), err)
					}
					assertEntry(entries[0])
					entry, err := s.GetRouteDecision(id)
					if err != nil {
						t.Fatalf("GetRouteDecision: %v", err)
					}
					assertEntry(entry)
					entry, err = s.GetRouteDecisionByRequestID(requestID)
					if err != nil {
						t.Fatalf("GetRouteDecisionByRequestID: %v", err)
					}
					assertEntry(entry)
				})
			}

			// Reproduce replicated migration bookkeeping without its local DDL.
			var migrated, indexed bool
			if err := s.queryRow(`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?),
				to_regclass('idx_attempts_request_text') IS NOT NULL`, migration).Scan(&migrated, &indexed); err != nil {
				t.Fatal(err)
			}
			if !migrated || !indexed {
				t.Fatalf("initial migration: version=%v index=%v", migrated, indexed)
			}
			if _, err := s.exec(`DROP INDEX idx_attempts_request_text`); err != nil {
				t.Fatal(err)
			}
			if err := s.queryRow(`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?),
				to_regclass('idx_attempts_request_text') IS NOT NULL`, migration).Scan(&migrated, &indexed); err != nil {
				t.Fatal(err)
			}
			if !migrated || indexed {
				t.Fatalf("missing local DDL fixture: version=%v index=%v", migrated, indexed)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dsn)
			if err != nil {
				t.Fatalf("reopen with replicated migration row: %v", err)
			}
			if err := s.queryRow(`SELECT to_regclass('idx_attempts_request_text') IS NOT NULL`).Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			if !indexed {
				t.Fatal("reopen did not restore the missing local expression index")
			}

			// Deterministic, production-sized attempt history: 10,000 requests with
			// four attempts each. Do not disable sequential scans or time the query.
			if _, err := s.exec(`INSERT INTO requests(request_id,model,outcome,created_at,completed_at)
				SELECT md5('route-plan-' || n::text)::uuid,'pg-route-plan','success',
					TIMESTAMPTZ '2026-01-01 00:00:00+00' + n * INTERVAL '1 second',
					TIMESTAMPTZ '2026-01-01 00:00:00+00' + n * INTERVAL '1 second'
				FROM generate_series(1,10000) AS series(n)`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.exec(`INSERT INTO request_attempts(request_id,attempt_no,protocol,outcome,created_at,completed_at)
				SELECT r.request_id,a.n,CASE WHEN a.n=4 THEN 'responses' ELSE 'claude' END,
					'success',r.created_at,r.completed_at
				FROM requests r CROSS JOIN generate_series(1,4) AS a(n)
				WHERE r.model='pg-route-plan' ORDER BY r.id,a.n`); err != nil {
				t.Fatal(err)
			}
			requestIDExpr := "request_id"
			if shape.legacyText {
				requestIDExpr = "request_id::text"
			}
			if _, err := s.exec(`INSERT INTO route_decisions(request_id,model,protocol,actual_input_tokens,actual_cached_tokens,created_at)
				SELECT ` + requestIDExpr + `,'pg-route-plan','claude',1000,900,created_at
				FROM requests WHERE model='pg-route-plan' ORDER BY id DESC LIMIT 1000`); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"requests", "request_attempts", "route_decisions", "upstreams"} {
				if _, err := s.exec("ANALYZE " + table); err != nil {
					t.Fatal(err)
				}
			}
			for _, limit := range []int{50, 500} {
				t.Run(fmt.Sprintf("indexed_history_%d", limit), func(t *testing.T) {
					entries, err := s.ListRouteDecisions(RouteDecisionFilter{Limit: limit})
					if err != nil || len(entries) != limit {
						t.Fatalf("history: entries=%d err=%v", len(entries), err)
					}
					for _, entry := range entries {
						if entry.TokenAccounting == nil || entry.TokenAccounting.Report.Protocol != "responses" || entry.ActualPromptTokens == nil || *entry.ActualPromptTokens != 1000 {
							t.Fatalf("history lost latest-attempt semantics: %+v", entry)
						}
					}
					query := s.routeDecisionSelect(" WHERE 1=1") + " ORDER BY rd.id DESC LIMIT ?"
					var raw []byte
					if err := s.queryRow("EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+query, limit).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plans []struct {
						Plan postgresRouteDecisionPlan
					}
					if err := json.Unmarshal(raw, &plans); err != nil {
						t.Fatal(err)
					}
					if len(plans) != 1 {
						t.Fatalf("unexpected EXPLAIN document: %s", raw)
					}
					lookups := 0
					var inspect func(postgresRouteDecisionPlan)
					inspect = func(node postgresRouteDecisionPlan) {
						if node.RelationName == "request_attempts" {
							if (node.NodeType != "Index Scan" && node.NodeType != "Index Only Scan") ||
								node.IndexName != "idx_attempts_request_text" || !strings.Contains(node.IndexCond, "rd.request_id") {
								t.Fatalf("attempt history must use a parameterized expression-index lookup: %+v\n%s", node, raw)
							}
							if node.ActualLoops != float64(limit) || node.ActualRows != 1 {
								t.Fatalf("want one latest-attempt row per decision: %+v\n%s", node, raw)
							}
							lookups++
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plans[0].Plan)
					if lookups == 0 {
						t.Fatalf("no parameterized request_attempts lookup in real route-decision plan: %s", raw)
					}
				})
			}
		})
	}
}

// Only execution-plan properties needed to reject repeated whole-history scans.
type postgresRouteDecisionPlan struct {
	NodeType     string                      `json:"Node Type"`
	RelationName string                      `json:"Relation Name"`
	IndexName    string                      `json:"Index Name"`
	IndexCond    string                      `json:"Index Cond"`
	ActualLoops  float64                     `json:"Actual Loops"`
	ActualRows   float64                     `json:"Actual Rows"`
	Plans        []postgresRouteDecisionPlan `json:"Plans"`
}
