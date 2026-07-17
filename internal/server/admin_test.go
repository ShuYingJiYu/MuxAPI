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

// newAdminTestServer 起一个带固定 admin token 的后台测试服务。
func newAdminTestServer(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	hm := health.New(1, time.Hour)
	sched := scheduler.New(func(int64) []*upstream.Upstream { return nil }, hm)
	fwd := forward.New(sched, hm, 3)
	const tok = "admin-tok"
	srv := New(fwd, tok, st, hm, monitor.New(st), nil, 32<<20)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, tok
}

func adminReq(t *testing.T, method, url, tok, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSettingsFirstResponseTimeout(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	url := ts.URL + "/admin/settings"

	get := adminReq(t, http.MethodGet, url, tok, "")
	defer get.Body.Close()
	var defaults map[string]string
	if err := json.NewDecoder(get.Body).Decode(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults["effective_first_response_timeout_ms"] != "120000" || defaults["first_response_timeout_ms_source"] != "default" {
		t.Fatalf("unexpected first-response defaults: %+v", defaults)
	}

	put := adminReq(t, http.MethodPut, url, tok, `{
		"log_retention":"7",
		"alert_webhook":"",
		"alert_debounce":"60s",
		"first_response_timeout_ms":"180000",
		"route_smart":"on"
	}`)
	put.Body.Close()
	if put.StatusCode != http.StatusNoContent {
		t.Fatalf("save settings returned %d", put.StatusCode)
	}
	if got := st.GetSetting("first_response_timeout_ms", ""); got != "180000" {
		t.Fatalf("first response timeout was not persisted: %q", got)
	}
}

// L13 回归：布尔启停 PUT 收到空/畸形 body 必须 400，绝不静默写 enabled=false。
func TestKeyEnabledRejectsMalformedBody(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	gid, _ := st.CreateGroup("g", "")
	st.CreateKey("k", gid)
	ks, _ := st.ListKeys(gid)
	if len(ks) == 0 {
		t.Fatal("应有一把 key")
	}
	url := ts.URL + "/admin/keys/" + itoa(ks[0].ID)

	// 畸形 JSON
	resp := adminReq(t, http.MethodPut, url, tok, `{bad json`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("畸形 body 应 400，实际 %d", resp.StatusCode)
	}
	// 空 body
	resp2 := adminReq(t, http.MethodPut, url, tok, ``)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 body 应 400，实际 %d", resp2.StatusCode)
	}
	// 合法 body 仍正常（204）
	resp3 := adminReq(t, http.MethodPut, url, tok, `{"enabled":true}`)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("合法 body 应 204，实际 %d", resp3.StatusCode)
	}
}

// L13 回归：组内成员启停 PUT 同样拒绝畸形 body。
func TestMemberEnabledRejectsMalformedBody(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://x", Enabled: true})
	ups, _ := st.List()
	gid, _ := st.CreateGroup("g", "")
	st.AddMember(gid, ups[0].ID, 1, 1)
	url := ts.URL + "/admin/groups/" + itoa(gid) + "/upstreams/" + itoa(ups[0].ID)

	resp := adminReq(t, http.MethodPut, url, tok, `{bad`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("组内启停畸形 body 应 400，实际 %d", resp.StatusCode)
	}
}

// L14 回归：创建上游必须校验 Name 非空、BaseURL 为合法 http(s) URL。
func TestCreateUpstreamValidatesInput(t *testing.T) {
	ts, _, tok := newAdminTestServer(t)
	url := ts.URL + "/admin/upstreams"

	cases := []struct {
		name string
		body string
	}{
		{"空 name", `{"name":"","base_url":"http://ok"}`},
		{"空白 name", `{"name":"   ","base_url":"http://ok"}`},
		{"空 base_url", `{"name":"x","base_url":""}`},
		{"非法 scheme", `{"name":"x","base_url":"ftp://h"}`},
		{"无 host", `{"name":"x","base_url":"http://"}`},
		{"非 URL", `{"name":"x","base_url":"::::"}`},
	}
	for _, c := range cases {
		resp := adminReq(t, http.MethodPost, url, tok, c.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d", c.name, resp.StatusCode)
		}
	}

	// 合法输入 → 201
	ok := adminReq(t, http.MethodPost, url, tok, `{"name":"good","base_url":"https://api.example.com"}`)
	ok.Body.Close()
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("合法上游应 201，实际 %d", ok.StatusCode)
	}
}

func TestUpstreamSourceAndBatchAPI(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	create := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams", tok,
		`{"name":"relay-a","source":" Relay ","base_url":"https://api.example.com","api_key":"secret","enabled":true}`)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d", create.StatusCode)
	}
	list, _ := st.List()
	if len(list) != 1 || list[0].Source != "Relay" {
		t.Fatalf("source was not normalized: %+v", list)
	}

	batch := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams/batch", tok,
		`{"ids":[`+itoa(list[0].ID)+`],"source":"Backup","enabled":false}`)
	batch.Body.Close()
	if batch.StatusCode != http.StatusNoContent {
		t.Fatalf("batch returned %d", batch.StatusCode)
	}
	updated, _ := st.Get(list[0].ID)
	if updated.Source != "Backup" || updated.Enabled || updated.APIKey != "secret" {
		t.Fatalf("unexpected batch result: %+v", updated)
	}

	bad := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams/batch", tok, `{"ids":[]}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty batch should return 400, got %d", bad.StatusCode)
	}
}

// L15 回归：创建监控引用不存在的 upstream_id 必须 404，不产生孤儿监控行。
func TestCreateMonitorRejectsMissingUpstream(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	url := ts.URL + "/admin/monitors"

	// upstream 999 不存在
	resp := adminReq(t, http.MethodPost, url, tok, `{"upstream_id":999,"model":"gpt-4o"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("引用不存在上游应 404，实际 %d", resp.StatusCode)
	}
	// 不应留下孤儿行
	ms, _ := st.ListMonitors(false)
	if len(ms) != 0 {
		t.Fatalf("不应创建监控行，实际 %d 条", len(ms))
	}

	// 真实 upstream → 成功建监控
	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://x", Enabled: true})
	ups, _ := st.List()
	ok := adminReq(t, http.MethodPost, url, tok, `{"upstream_id":`+itoa(ups[0].ID)+`,"model":"gpt-4o"}`)
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("合法监控应 200，实际 %d", ok.StatusCode)
	}
}

// itoa 小工具：int64 转十进制字符串（避免在每个用例 import strconv）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
