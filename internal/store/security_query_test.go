package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSecurityQueryViewsAndSummary(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().Add(-time.Hour).Unix()
	rows := []SecurityLog{
		{ID: 1, AggregationKey: "1", Kind: "login", Success: true, Count: 2},
		{ID: 2, AggregationKey: "2", Kind: "access", Success: true, Count: 100},
		{ID: 3, AggregationKey: "3", Kind: "access", Count: 3},
		{ID: 4, AggregationKey: "4", Kind: "access", Success: true, Count: 1, NewBrowser: true},
		{ID: 5, AggregationKey: "5", Kind: "login", Count: 1},
		{ID: 6, AggregationKey: "6", Kind: "login", Count: 200, NewSource: true, ClientIP: "192.0.2.2"},
		{ID: 7, AggregationKey: "7", Kind: "login", Count: 200, FirstAt: at - 16*86400},
	}
	for i := range rows {
		if rows[i].ClientIP == "" {
			rows[i].ClientIP = "192.0.2.1"
		}
		if rows[i].FirstAt == 0 {
			rows[i].FirstAt = at + int64(i)
		}
		rows[i].LastAt = rows[i].FirstAt
	}
	if err := st.gormDB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	wantSummary := SecuritySummary{Failures: 4, AttentionIPs: 1, LatestSuccess: at + 3,
		LoginSuccesses: 2, ManagementAccesses: 104, LatestLogin: at}
	for _, tc := range []struct {
		name, view, kind, result string
		ids                      []int64
	}{
		{"legacy all", "", "", "", []int64{5, 4, 3, 2, 1}},
		{"events", "events", "", "", []int64{5, 4, 3, 1}},
		{"all login", "all", "login", "", []int64{5, 1}},
		{"event access", "events", "access", "", []int64{4, 3}},
		{"event success", "events", "", "success", []int64{4, 1}},
		{"event failure", "events", "", "failure", []int64{5, 3}},
		{"attention", "all", "", "attention", []int64{4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := st.SecurityLogs(context.Background(), SecurityFilter{IP: "192.0.2.1", View: tc.view, Kind: tc.kind, Result: tc.result, Page: 1, Size: 20})
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			for _, row := range page.Entries {
				got = append(got, row.ID)
			}
			if !reflect.DeepEqual(got, tc.ids) || page.Total != int64(len(tc.ids)) {
				t.Fatalf("unexpected page: %+v", page)
			}
			if page.Summary != wantSummary {
				t.Fatalf("overview changed with list filter: got %+v want %+v", page.Summary, wantSummary)
			}
		})
	}
	page, err := st.SecurityLogs(context.Background(), SecurityFilter{IP: "192.0.2.1", View: "events", Page: 999, Size: 2})
	if err != nil || page.Page != 2 || page.Size != 2 || len(page.Entries) != 2 || page.Entries[0].ID != 3 || page.Entries[1].ID != 1 {
		t.Fatalf("page normalization: %+v %v", page, err)
	}
	page, err = st.SecurityLogs(context.Background(), SecurityFilter{IP: "192.0.2.99", Page: 999, Size: 50})
	if err != nil || page.Page != 1 || page.Total != 0 || len(page.Entries) != 0 || page.Summary != (SecuritySummary{}) {
		t.Fatalf("empty page: %+v %v", page, err)
	}
	page, err = st.SecurityLogs(context.Background(), SecurityFilter{IP: "192.0.2.1", From: at + 2, To: at + 3, Size: 20})
	if err != nil || page.Total != 2 || page.From != at+2 || page.To != at+3 || page.Summary.Failures != 3 || page.Summary.LoginSuccesses != 0 {
		t.Fatalf("time bounds: %+v %v", page, err)
	}
}
