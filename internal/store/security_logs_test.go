package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func flushSecurity(t *testing.T, st *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.FlushSecurity(ctx); err != nil {
		t.Fatal(err)
	}
}

func securityPage(t *testing.T, st *Store, filter SecurityFilter) SecurityPage {
	t.Helper()
	filter.Size = 100
	page, err := st.SecurityLogs(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestSecurityMergeAndFailureWindow(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().Add(-2*time.Minute).Unix() / 60 * 60
	for i := 0; i < 10; i++ {
		st.EnqueueSecurity(SecurityLog{FirstAt: at + int64(i), ClientIP: "192.0.2.1", Kind: "access", UserAgent: fmt.Sprint(i % 2)})
	}
	flushSecurity(t, st)
	page := securityPage(t, st, SecurityFilter{})
	if page.Total != 2 || page.Summary.Failures != 10 || page.Summary.AttentionIPs != 1 {
		t.Fatalf("failure aggregation: %+v", page)
	}
	for i := 0; i < 3; i++ {
		st.EnqueueSecurity(SecurityLog{FirstAt: at + 30 + int64(i), ClientIP: "192.0.2.1", Kind: "access", Success: true, UserAgent: "browser"})
	}
	flushSecurity(t, st)
	page = securityPage(t, st, SecurityFilter{Result: "success"})
	if len(page.Entries) != 1 || page.Entries[0].Count != 3 || !page.Entries[0].NewSource || !page.Entries[0].AfterFailures {
		t.Fatalf("success aggregation: %+v", page)
	}
	st.EnqueueSecurity(SecurityLog{FirstAt: at + 40, ClientIP: "192.0.2.1", Kind: "access", Success: true, UserAgent: "browser"})
	flushSecurity(t, st)
	page = securityPage(t, st, SecurityFilter{Result: "success"})
	if page.Entries[0].Count != 4 || page.Entries[0].LastAt != at+40 || !page.Entries[0].NewSource {
		t.Fatalf("merge across batches: %+v", page)
	}
}

func TestSecuritySourceHistoryAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "security.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Unix()
	st.EnqueueSecurity(SecurityLog{FirstAt: at, ClientIP: "192.0.2.2", Success: true, UserAgent: "browser"})
	st.EnqueueSecurity(SecurityLog{FirstAt: time.Now().Add(-16 * 24 * time.Hour).Unix(), ClientIP: "192.0.2.3", Success: true})
	flushSecurity(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.EnqueueSecurity(SecurityLog{FirstAt: at + 120, ClientIP: "192.0.2.2", Success: true, UserAgent: "browser"})
	st.EnqueueSecurity(SecurityLog{FirstAt: at + 180, ClientIP: "192.0.2.2", Success: true, UserAgent: "new browser"})
	st.EnqueueSecurity(SecurityLog{FirstAt: at + 240, ClientIP: "192.0.2.3", Success: true})
	flushSecurity(t, st)
	page := securityPage(t, st, SecurityFilter{IP: "192.0.2.2"})
	if page.Total != 3 || !page.Entries[0].NewBrowser || page.Entries[1].NewSource || page.Entries[1].NewBrowser {
		t.Fatalf("persistent source history: %+v", page)
	}
	page = securityPage(t, st, SecurityFilter{IP: "192.0.2.3"})
	if page.Total != 1 || !page.Entries[0].NewSource {
		t.Fatalf("history must expire: %+v", page)
	}
	deleted, err := st.PruneSecurityLogs(context.Background())
	if err != nil || deleted != 1 {
		t.Fatalf("retention: %d %v", deleted, err)
	}
}

func TestSecuritySanitizationAndWriteFailure(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.EnqueueSecurity(SecurityLog{FirstAt: time.Now().Unix(), UserAgent: "a\n\x00\u202e" + strings.Repeat("b", 500)})
	flushSecurity(t, st)
	row := securityPage(t, st, SecurityFilter{}).Entries[0]
	if strings.ContainsAny(row.UserAgent, "\n\x00\u202e") || len(row.UserAgent) != 256 {
		t.Fatal("unsafe UA")
	}
	if err := st.gormDB.Exec("DROP TABLE security_logs").Error; err != nil {
		t.Fatal(err)
	}
	st.EnqueueSecurity(SecurityLog{FirstAt: time.Now().Unix()})
	flushSecurity(t, st)
	dropped, failed := st.SecurityLogStatus()
	if dropped != 1 || !failed {
		t.Fatalf("missing write failure status: %d %t", dropped, failed)
	}
}

func TestSecurityBoundedBatchAndConcurrentClose(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix()
	for i := 0; i < 300; i++ {
		st.EnqueueSecurity(SecurityLog{FirstAt: at, UserAgent: fmt.Sprint(i)})
	}
	flushSecurity(t, st)
	drops, _ := st.SecurityLogStatus()
	if drops == 0 {
		t.Fatal("unbounded unique event batch")
	}
	st.EnqueueSecurity(SecurityLog{FirstAt: at, UserAgent: "another failure"})
	// 失败额度耗尽后，已知组合仍合并，新成功组合仍有独立额度。
	st.EnqueueSecurity(SecurityLog{FirstAt: at, UserAgent: "0"})
	st.EnqueueSecurity(SecurityLog{FirstAt: at, UserAgent: "browser", Success: true})
	flushSecurity(t, st)
	after, _ := st.SecurityLogStatus()
	page := securityPage(t, st, SecurityFilter{Result: "success"})
	if after != drops+1 || page.Total != 1 {
		t.Fatalf("minute admission: drops=%d after=%d page=%+v", drops, after, page)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				st.EnqueueSecurity(SecurityLog{FirstAt: time.Now().Unix()})
			}
		})
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if st.EnqueueSecurity(SecurityLog{}) {
		t.Fatal("enqueue after close")
	}
}

func TestSecurityBatchOrderAndExpiredFailureWindow(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().Add(-10*time.Minute).Unix() / 60 * 60
	// 成功记录先进入聚合器，但同一分钟内后续成功仍须识别中途的失败。
	for _, event := range []struct {
		second  int64
		success bool
	}{{1, true}, {20, false}, {40, true}, {50, false}} {
		st.EnqueueSecurity(SecurityLog{FirstAt: at + event.second, ClientIP: "192.0.2.4", Success: event.success})
	}
	// 不能把成功以后的失败归因为「失败后成功」。
	st.EnqueueSecurity(SecurityLog{FirstAt: at + 1, ClientIP: "192.0.2.5", Success: true})
	st.EnqueueSecurity(SecurityLog{FirstAt: at + 20, ClientIP: "192.0.2.5"})
	flushSecurity(t, st)
	page := securityPage(t, st, SecurityFilter{IP: "192.0.2.4", Result: "success"})
	if len(page.Entries) != 1 || !page.Entries[0].AfterFailures {
		t.Fatalf("missing intra-batch failure: %+v", page)
	}
	page = securityPage(t, st, SecurityFilter{IP: "192.0.2.5", Result: "success"})
	if page.Entries[0].AfterFailures {
		t.Fatal("future failure marked as preceding success")
	}
	for i := 0; i < 9; i++ {
		st.EnqueueSecurity(SecurityLog{FirstAt: at, ClientIP: "192.0.2.6"})
	}
	flushSecurity(t, st)
	st.EnqueueSecurity(SecurityLog{FirstAt: at + 300, ClientIP: "192.0.2.6"})
	flushSecurity(t, st)
	page = securityPage(t, st, SecurityFilter{IP: "192.0.2.6"})
	if page.Entries[0].Frequent {
		t.Fatal("expired failures counted in current five-minute buckets")
	}
	page = securityPage(t, st, SecurityFilter{IP: "192.0.2.5", Result: "attention"})
	if page.Total != 1 || page.Entries[0].ClientIP != "192.0.2.5" {
		t.Fatalf("attention filter escaped IP scope: %+v", page)
	}
}
