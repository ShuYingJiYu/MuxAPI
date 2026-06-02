package server

import (
	"io"
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

// 端到端：真实 HTTP 上游，验证转发 + 双 header 注入 + 故障切换。
func TestEndToEndForward(t *testing.T) {
	var gotAuth, gotKey string
	// 上游 A：始终 500（模拟挂掉）
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer upA.Close()
	// 上游 B：正常返回，并记录收到的 header
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upB.Close()

	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", BaseURL: upA.URL, APIKey: "keyA", Priority: 1, Enabled: true},
		{ID: 2, Name: "B", BaseURL: upB.URL, APIKey: "keyB", Priority: 2, Enabled: true},
	}
	hm := health.New(1, time.Hour)
	sched := scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm)

	// 真实 SQLite 内存库，建分组 + 系统生成 access_key 让请求通过分组路由
	st, _ := store.Open(":memory:")
	defer st.Close()
	fwd := forward.New(sched, hm, st, 3)
	gid, _ := st.CreateGroup("test", "")
	key, _ := st.CreateKey("test-key", gid)
	srv := New(fwd, "", st, hm, monitor.New(), nil)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 请求带 access key：A 挂 → 自动切到 B → B 返回 ok
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"x","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("A 挂应切到 B 返回 ok，实际 status=%d body=%s", resp.StatusCode, body)
	}
	// 验证双 header 注入了 B 的 key
	if gotAuth != "Bearer keyB" || gotKey != "keyB" {
		t.Fatalf("双 header 注入错误: Authorization=%q x-api-key=%q", gotAuth, gotKey)
	}
}
