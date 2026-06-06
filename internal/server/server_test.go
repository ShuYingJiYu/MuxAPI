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
	srv := New(fwd, "", st, hm, monitor.New(st), nil)

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

// /v1/models：汇总分组内各上游的模型并集；未知 key 返回 401。
func TestListModels(t *testing.T) {
	// 两个伪上游各返回不同模型（含一个重复，验证去重）
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"shared"}]}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"claude-3"},{"id":"shared"}]}`))
	}))
	defer upB.Close()

	st, _ := store.Open(":memory:")
	defer st.Close()
	st.Create(&upstream.Upstream{Name: "A", BaseURL: upA.URL, APIKey: "kA", Enabled: true})
	st.Create(&upstream.Upstream{Name: "B", BaseURL: upB.URL, APIKey: "kB", Enabled: true})
	ups, _ := st.List()
	gid, _ := st.CreateGroup("g", "")
	for _, u := range ups {
		st.AddMember(gid, u.ID, 1, 1)
	}
	key, _ := st.CreateKey("k", gid)

	hm := health.New(1, time.Hour)
	sched := scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm)
	fwd := forward.New(sched, hm, st, 3)
	srv := New(fwd, "", st, hm, monitor.New(st), nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 未知 key → 401
	bad, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	if resp, _ := http.DefaultClient.Do(bad); resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未知 key 应 401，实际 %v", resp)
	}

	// 已知 key → 并集去重，含 gpt-4o/claude-3/shared
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{`"object":"list"`, "gpt-4o", "claude-3", "shared"} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %q，body=%s", want, s)
		}
	}
	if strings.Count(s, `"id":"shared"`) != 1 {
		t.Fatalf("shared 应去重为 1 个，body=%s", s)
	}
}
