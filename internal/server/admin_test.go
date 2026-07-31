package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/billing"
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
	srv.SetBillingManager(billing.NewManager(st))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, tok
}

func TestUpstreamBillingRefreshAndList(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.Write([]byte(`{"remaining":8.75,"unit":"USD","usage":{"total":{"cost":30,"actual_cost":4.5}}}`))
		case "/v1/sub2api/billing":
			w.Write([]byte(`{"object":"sub2api.key_billing","group_rate_multiplier":0.15,"effective_rate_multiplier":0.15}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	ts, st, tok := newAdminTestServer(t)
	u := &upstream.Upstream{
		Name: "provider", BaseURL: provider.URL, APIKey: "sk-test",
		BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}

	refresh := adminReq(t, http.MethodPost,
		ts.URL+"/admin/upstreams/"+itoa(u.ID)+"/billing/refresh", tok, "")
	defer refresh.Body.Close()
	if refresh.StatusCode != http.StatusOK {
		t.Fatalf("billing refresh returned %d", refresh.StatusCode)
	}
	var refreshed store.BillingStatus
	if err := json.NewDecoder(refresh.Body).Decode(&refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != "ok" || refreshed.Remaining == nil || *refreshed.Remaining != 8.75 {
		t.Fatalf("unexpected refreshed billing state: %+v", refreshed)
	}

	list := adminReq(t, http.MethodGet, ts.URL+"/admin/upstreams", tok, "")
	defer list.Body.Close()
	var items []upstreamDTO
	if err := json.NewDecoder(list.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].BillingType != upstream.BillingSub2API ||
		items[0].Billing == nil || items[0].Billing.Remaining == nil || *items[0].Billing.Remaining != 8.75 {
		t.Fatalf("upstream list is missing billing data: %+v", items)
	}
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

func TestLogCacheStatsAPI(t *testing.T) {
	ts, _, tok := newAdminTestServer(t)
	resp := adminReq(t, http.MethodGet, ts.URL+"/admin/logs/cache-stats", tok, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache stats returned %d", resp.StatusCode)
	}
	var stats []store.ChannelCacheStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("new store should have no cache stats: %+v", stats)
	}
}

func TestReorderGroupsAPI(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	id1, _ := st.CreateGroup("g1", "")
	id2, _ := st.CreateGroup("g2", "")
	id3, _ := st.CreateGroup("g3", "")

	resp := adminReq(t, http.MethodPost, ts.URL+"/admin/groups/reorder", tok,
		`{"ids":[`+itoa(id3)+`,`+itoa(id1)+`,`+itoa(id2)+`]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reorder groups returned %d", resp.StatusCode)
	}

	groups, err := st.ListGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 || groups[0].ID != id3 || groups[1].ID != id1 || groups[2].ID != id2 {
		t.Fatalf("unexpected group order: %+v", groups)
	}
}

func TestGroupMaxMultiplierAPI(t *testing.T) {
	ts, _, tok := newAdminTestServer(t)
	create := adminReq(t, http.MethodPost, ts.URL+"/admin/groups", tok,
		`{"name":"cost limited","description":"test","max_multiplier":0.2}`)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create group returned %d", create.StatusCode)
	}

	list := adminReq(t, http.MethodGet, ts.URL+"/admin/groups", tok, "")
	var groups []struct {
		ID            int64    `json:"id"`
		MaxMultiplier *float64 `json:"max_multiplier"`
	}
	if err := json.NewDecoder(list.Body).Decode(&groups); err != nil {
		t.Fatal(err)
	}
	list.Body.Close()
	if len(groups) != 1 || groups[0].MaxMultiplier == nil || *groups[0].MaxMultiplier != 0.2 {
		t.Fatalf("group list is missing multiplier limit: %+v", groups)
	}

	update := adminReq(t, http.MethodPut, ts.URL+"/admin/groups/"+itoa(groups[0].ID), tok,
		`{"name":"cost limited","description":"updated","max_multiplier":0.15}`)
	update.Body.Close()
	if update.StatusCode != http.StatusNoContent {
		t.Fatalf("update group returned %d", update.StatusCode)
	}
	bad := adminReq(t, http.MethodPost, ts.URL+"/admin/groups", tok,
		`{"name":"invalid","max_multiplier":0}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero multiplier limit returned %d", bad.StatusCode)
	}
}

func TestEffectivePrioritySkipsMultiplierBlockedMembers(t *testing.T) {
	members := []*store.Member{
		{UpstreamID: 1, Priority: 1, Enabled: true, GroupEnabled: true, MultiplierBlocked: true},
		{UpstreamID: 2, Priority: 2, Enabled: true, GroupEnabled: true},
	}
	priority, ok := effectivePriority(members, func(int64) string { return "CLOSED" })
	if !ok || priority != 2 {
		t.Fatalf("effective priority = %d, %v", priority, ok)
	}
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
		"first_response_timeout_ms":"180000"
	}`)
	put.Body.Close()
	if put.StatusCode != http.StatusNoContent {
		t.Fatalf("save settings returned %d", put.StatusCode)
	}
	if got := st.GetSetting("first_response_timeout_ms", ""); got != "180000" {
		t.Fatalf("first response timeout was not persisted: %q", got)
	}
}

func TestRuntimeRoutingSettings(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	url := ts.URL + "/admin/settings"
	put := adminReq(t, http.MethodPut, url, tok, `{
		"log_retention":"7",
		"alert_webhook":"",
		"alert_debounce":"60s",
		"first_response_timeout_ms":"120000",
		"fail_threshold":"4",
		"cooldown":"2m",
		"max_upstream_attempts":"8",
		"max_body_bytes":"67108864"
	}`)
	put.Body.Close()
	if put.StatusCode != http.StatusNoContent {
		t.Fatalf("save runtime settings returned %d", put.StatusCode)
	}
	for key, want := range map[string]string{
		"fail_threshold": "4", "cooldown": "2m",
		"max_upstream_attempts": "8", "max_body_bytes": "67108864",
	} {
		if got := st.GetSetting(key, ""); got != want {
			t.Fatalf("setting %s = %q, want %q", key, got, want)
		}
	}

	get := adminReq(t, http.MethodGet, url, tok, "")
	defer get.Body.Close()
	var settings map[string]string
	if err := json.NewDecoder(get.Body).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	if settings["effective_max_upstream_attempts"] != "8" || settings["max_upstream_attempts_source"] != "settings" {
		t.Fatalf("unexpected effective runtime settings: %+v", settings)
	}

	bad := adminReq(t, http.MethodPut, url, tok, `{"max_upstream_attempts":"101"}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid max attempts returned %d", bad.StatusCode)
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
	tag := adminReq(t, http.MethodPost, ts.URL+"/admin/tags", tok, `{"name":"Backup","color":"blue"}`)
	var createdTag map[string]int64
	if err := json.NewDecoder(tag.Body).Decode(&createdTag); err != nil {
		t.Fatal(err)
	}
	tag.Body.Close()
	if tag.StatusCode != http.StatusCreated || createdTag["id"] == 0 {
		t.Fatalf("tag create returned %d: %+v", tag.StatusCode, createdTag)
	}
	create := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams", tok,
		`{"name":"relay-a","base_url":"https://api.example.com","api_key":"secret","enabled":true,"primary_tag_id":`+itoa(createdTag["id"])+`,"tag_ids":[]}`)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d", create.StatusCode)
	}
	list, _ := st.List()
	if len(list) != 1 || list[0].Source != "Backup" || list[0].PrimaryTagID != createdTag["id"] {
		t.Fatalf("primary tag was not saved: %+v", list)
	}
	get := adminReq(t, http.MethodGet, ts.URL+"/admin/upstreams", tok, "")
	var upstreams []upstreamDTO
	if err := json.NewDecoder(get.Body).Decode(&upstreams); err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if len(upstreams) != 1 || upstreams[0].PrimaryTagID != createdTag["id"] || len(upstreams[0].Tags) != 1 || !upstreams[0].Tags[0].IsPrimary {
		t.Fatalf("admin upstream tags missing: %+v", upstreams)
	}

	batch := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams/batch", tok,
		`{"ids":[`+itoa(list[0].ID)+`],"primary_tag_id":0,"enabled":false}`)
	batch.Body.Close()
	if batch.StatusCode != http.StatusNoContent {
		t.Fatalf("batch returned %d", batch.StatusCode)
	}
	updated, _ := st.Get(list[0].ID)
	if updated.Source != "" || updated.PrimaryTagID != 0 || updated.Enabled || updated.APIKey != "secret" {
		t.Fatalf("unexpected batch result: %+v", updated)
	}

	bad := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams/batch", tok, `{"ids":[]}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty batch should return 400, got %d", bad.StatusCode)
	}
}

func TestRecoverUpstreamAPI(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hm := health.New(1, time.Hour)
	sched := scheduler.New(func(int64) []*upstream.Upstream { return nil }, hm)
	const token = "admin-tok"
	ts := httptest.NewServer(New(forward.New(sched, hm, 1), token, st, hm, monitor.New(st), nil, 32<<20).Handler())
	defer ts.Close()

	if err := st.Create(&upstream.Upstream{Name: "relay", BaseURL: "https://api.example.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	list, _ := st.List()
	id := list[0].ID
	hm.Report(id, "gpt", false, 0)
	if got := hm.EffectiveState(id); got != "OPEN" {
		t.Fatalf("precondition state = %s, want OPEN", got)
	}

	resp := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams/"+itoa(id)+"/recover", token, "")
	defer resp.Body.Close()
	var recovered healthView
	if err := json.NewDecoder(resp.Body).Decode(&recovered); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || recovered.State != "CLOSED" || recovered.Fails != 0 {
		t.Fatalf("recover returned status=%d body=%+v", resp.StatusCode, recovered)
	}
	if !hm.IsAvailable(id, "gpt") {
		t.Fatal("recovered upstream should be available")
	}

	missing := adminReq(t, http.MethodPost, ts.URL+"/admin/upstreams/999/recover", token, "")
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing upstream returned %d", missing.StatusCode)
	}
}

func TestRecoverUpstreamModelAPI(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hm := health.New(1, time.Hour)
	sched := scheduler.New(func(int64) []*upstream.Upstream { return nil }, hm)
	const token = "admin-tok"
	ts := httptest.NewServer(New(forward.New(sched, hm, 1), token, st, hm, monitor.New(st), nil, 32<<20).Handler())
	defer ts.Close()

	item := &upstream.Upstream{Name: "relay", BaseURL: "https://api.example.com", Enabled: true}
	if err := st.Create(item); err != nil {
		t.Fatal(err)
	}
	hm.MarkModelUnsupported(item.ID, "codex-auto-review")
	if hm.IsAvailable(item.ID, "codex-auto-review") {
		t.Fatal("precondition model should be temporarily excluded")
	}

	resp := adminReq(t, http.MethodPost,
		ts.URL+"/admin/upstreams/"+itoa(item.ID)+"/models/recover", token,
		`{"model":"codex-auto-review"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || !hm.IsAvailable(item.ID, "codex-auto-review") {
		t.Fatalf("model recover returned %d, available=%v", resp.StatusCode,
			hm.IsAvailable(item.ID, "codex-auto-review"))
	}

	bad := adminReq(t, http.MethodPost,
		ts.URL+"/admin/upstreams/"+itoa(item.ID)+"/models/recover", token, `{"model":""}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty model returned %d", bad.StatusCode)
	}
	missing := adminReq(t, http.MethodPost,
		ts.URL+"/admin/upstreams/999/models/recover", token, `{"model":"gpt"}`)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing upstream returned %d", missing.StatusCode)
	}
}

func TestTagAPIValidatesAndUpdates(t *testing.T) {
	ts, st, tok := newAdminTestServer(t)
	bad := adminReq(t, http.MethodPost, ts.URL+"/admin/tags", tok, `{"name":"","color":"orange"}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid tag should return 400, got %d", bad.StatusCode)
	}

	created := adminReq(t, http.MethodPost, ts.URL+"/admin/tags", tok, `{"name":"Coding","color":"orange"}`)
	var body map[string]int64
	if err := json.NewDecoder(created.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	id := body["id"]
	updated := adminReq(t, http.MethodPut, ts.URL+"/admin/tags/"+itoa(id), tok, `{"name":"Code","color":"teal"}`)
	updated.Body.Close()
	if updated.StatusCode != http.StatusNoContent {
		t.Fatalf("tag update returned %d", updated.StatusCode)
	}
	tags, _ := st.ListTags()
	if len(tags) != 1 || tags[0].Name != "Code" || tags[0].Color != "teal" {
		t.Fatalf("unexpected tags: %+v", tags)
	}
	deleted := adminReq(t, http.MethodDelete, ts.URL+"/admin/tags/"+itoa(id), tok, "")
	deleted.Body.Close()
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("tag delete returned %d", deleted.StatusCode)
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
