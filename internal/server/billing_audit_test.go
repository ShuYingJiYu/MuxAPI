package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

type auditWindowOption struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Seconds int64  `json:"seconds"`
}

type auditResponse struct {
	Window  string              `json:"window"`
	Label   string              `json:"label"`
	Windows []auditWindowOption `json:"windows"`
	Audit   store.BillingAudit  `json:"audit"`
}

func TestUpstreamBillingAuditWindowSelection(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	if err := st.Create(&upstream.Upstream{
		Name: "A", BaseURL: "http://x", APIKey: "k", Enabled: true,
		BillingType: upstream.BillingSub2API,
	}); err != nil {
		t.Fatal(err)
	}
	ups, _ := st.List()
	id := ups[0].ID

	// 未指定窗口 → 默认 24h
	resp := adminReq(t, http.MethodGet, ts.URL+"/admin/upstreams/"+itoa(id)+"/billing/audit", tok, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit returned %d", resp.StatusCode)
	}
	var payload auditResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Window != store.DefaultBillingWindow {
		t.Fatalf("default window = %q, want %q", payload.Window, store.DefaultBillingWindow)
	}
	if len(payload.Windows) != len(store.BillingWindows()) {
		t.Fatalf("window options = %d", len(payload.Windows))
	}
	// 无快照时应为 pending，而不是报错
	if payload.Audit.Status != "pending" {
		t.Fatalf("audit without snapshots should be pending: %+v", payload.Audit)
	}

	// 显式窗口生效
	resp7d := adminReq(t, http.MethodGet, ts.URL+"/admin/upstreams/"+itoa(id)+"/billing/audit?window=7d", tok, "")
	defer resp7d.Body.Close()
	var week auditResponse
	if err := json.NewDecoder(resp7d.Body).Decode(&week); err != nil {
		t.Fatal(err)
	}
	if week.Window != "7d" || week.Audit.WindowSeconds != 7*24*3600 {
		t.Fatalf("explicit window not honored: %+v", week)
	}

	// 不存在的上游 → 404
	missing := adminReq(t, http.MethodGet, ts.URL+"/admin/upstreams/999999/billing/audit", tok, "")
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown upstream = %d, want 404", missing.StatusCode)
	}
}
