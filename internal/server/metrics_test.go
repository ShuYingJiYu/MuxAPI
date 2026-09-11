package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestMetricsHandlerUsesDedicatedAuthAndPassiveSnapshot(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hm := health.New(1, time.Hour)
	sched := scheduler.New(func(int64) []*upstream.Upstream { return nil }, hm)
	srv := New(forward.New(sched, hm, 1), "admin-token", st, hm, monitor.New(st), nil, 1<<20)
	// Metrics are normally bound to a private listener and use their own token;
	// this also proves the business handler does not accidentally expose them.
	srv.SetMetricsToken("metrics-token")
	business := httptest.NewServer(srv.Handler())
	defer business.Close()
	if response, err := http.Get(business.URL + "/metrics"); err != nil {
		t.Fatal(err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("business listener /metrics status = %d, want 404", response.StatusCode)
		}
	}
	metrics := httptest.NewServer(srv.MetricsHandler())
	defer metrics.Close()
	unauthorized, err := http.Get(metrics.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized metrics status = %d, want 401", unauthorized.StatusCode)
	}
	postReq, _ := http.NewRequest(http.MethodPost, metrics.URL+"/metrics", nil)
	postReq.Header.Set("Authorization", "Bearer metrics-token")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("metrics POST status = %d, want 405", postResp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, metrics.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer metrics-token")
	authorized, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer authorized.Body.Close()
	if authorized.StatusCode != http.StatusOK || !strings.Contains(authorized.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("authorized metrics response: status=%d content-type=%q", authorized.StatusCode, authorized.Header.Get("Content-Type"))
	}

	srv.passive.Begin()
	srv.passive.Finish(forward.Result{Status: 200, Outcome: forward.OutcomeSuccess}, 12)
	admin := httptest.NewServer(srv.Handler())
	defer admin.Close()
	adminReq, _ := http.NewRequest(http.MethodGet, admin.URL+"/admin/passive", nil)
	adminReq.Header.Set("Authorization", "Bearer admin-token")
	adminResp, err := http.DefaultClient.Do(adminReq)
	if err != nil {
		t.Fatal(err)
	}
	defer adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("admin passive status = %d", adminResp.StatusCode)
	}
	var snapshot struct {
		Requests uint64 `json:"requests"`
		Attempts uint64 `json:"attempts"`
	}
	if err := json.NewDecoder(adminResp.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Requests != 1 || snapshot.Attempts != 0 {
		t.Fatalf("unexpected passive snapshot: %+v", snapshot)
	}
	historyReq, _ := http.NewRequest(http.MethodGet, admin.URL+"/admin/passive/history?window=24h", nil)
	historyReq.Header.Set("Authorization", "Bearer admin-token")
	historyResp, err := http.DefaultClient.Do(historyReq)
	if err != nil {
		t.Fatal(err)
	}
	defer historyResp.Body.Close()
	if historyResp.StatusCode != http.StatusOK {
		t.Fatalf("passive history status = %d", historyResp.StatusCode)
	}
	var history struct {
		Requests struct {
			Total int64 `json:"total"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(historyResp.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if history.Requests.Total != 0 {
		t.Fatalf("empty durable history = %+v", history)
	}
}

func TestParsePassiveWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"":    24 * time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"90m": 90 * time.Minute,
	}
	for input, want := range cases {
		got, err := parsePassiveWindow(input)
		if err != nil || got != want {
			t.Fatalf("parsePassiveWindow(%q) = %s, %v; want %s", input, got, err, want)
		}
	}
	for _, input := range []string{"0s", "31d", "bad"} {
		if _, err := parsePassiveWindow(input); err == nil {
			t.Fatalf("parsePassiveWindow(%q) should reject", input)
		}
	}
}
