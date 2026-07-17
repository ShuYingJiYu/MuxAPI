package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/translate"
	"github.com/mirainya/muxapi/internal/upstream"
)

var errBadMonitor = errors.New("monitor requires upstream_id and model")

func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin/upstreams", s.auth(s.adminUpstreams))     // GET 全局池 / POST 新增
	mux.HandleFunc("/admin/upstreams/", s.auth(s.adminUpstreamItem)) // PUT 改 / DELETE 删
	mux.HandleFunc("/admin/tags", s.auth(s.adminTags))               // GET 列表 / POST 新增
	mux.HandleFunc("/admin/tags/", s.auth(s.adminTagItem))           // PUT 改 / DELETE 删
	mux.HandleFunc("/admin/monitors", s.auth(s.adminMonitors))       // GET 监控列表 / POST 新增
	mux.HandleFunc("/admin/monitors/", s.auth(s.adminMonitorItem))   // PUT 改 / DELETE 删 / {id}/probe 立即探测
	mux.HandleFunc("/admin/groups", s.auth(s.adminGroups))           // GET 列表 / POST 新增
	mux.HandleFunc("/admin/groups/", s.auth(s.adminGroupSub))        // /{id} 改/删 ; /{id}/upstreams 成员 ; /{id}/keys 密钥
	mux.HandleFunc("/admin/keys/", s.auth(s.adminKeyItem))           // PUT 启停 / DELETE 删
	mux.HandleFunc("/admin/logs", s.auth(s.adminLogs))               // GET 调用日志(游标分页+筛选)
	mux.HandleFunc("/admin/logs/stats", s.auth(s.adminLogStats))     // GET 当前筛选范围统计
	mux.HandleFunc("/admin/logs/options", s.auth(s.adminLogOptions)) // GET 筛选下拉选项(全量去重)
	mux.HandleFunc("/admin/logs/", s.auth(s.adminLogItem))           // GET 单条请求完整尝试链
	mux.HandleFunc("/admin/settings", s.auth(s.adminSettings))       // GET/PUT 运行时设置
}

// adminLogs 返回调用日志，兼容游标和偏移量分页。
// 查询参: before(游标), offset(页偏移), limit(默认50,上限500)及业务筛选项。
func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	page, err := s.store.ListRequestsPage(parseLogFilter(r.URL.Query()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, page)
}

func parseLogFilter(q url.Values) store.RequestFilter {
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	upstreamID, _ := strconv.ParseInt(q.Get("upstream_id"), 10, 64)
	slowMs, _ := strconv.ParseInt(q.Get("slow_ms"), 10, 64)
	sinceUnix, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	untilUnix, _ := strconv.ParseInt(q.Get("until"), 10, 64)
	filter := store.RequestFilter{
		BeforeID: before, Limit: limit, Offset: offset, Model: q.Get("model"), Group: q.Get("group"),
		Status: q.Get("status"), KeyName: q.Get("key"), Endpoint: q.Get("endpoint"),
		ErrorKind: q.Get("error_kind"), Query: strings.TrimSpace(q.Get("q")),
		Stream: q.Get("stream"), UpstreamID: upstreamID, Retried: q.Get("retried") == "true",
		SlowMs: slowMs,
	}
	if sinceUnix > 0 {
		filter.Since = time.Unix(sinceUnix, 0)
	}
	if untilUnix > 0 {
		filter.Until = time.Unix(untilUnix, 0)
	}
	return filter
}

func (s *Server) adminLogStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.RequestStats(parseLogFilter(r.URL.Query()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, stats)
}

func (s *Server) adminLogItem(w http.ResponseWriter, r *http.Request) {
	idText := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/logs/"), "/")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid request record id", http.StatusBadRequest)
		return
	}
	entry, err := s.store.GetRequest(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "request record not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, entry)
}

// adminLogOptions 返回日志筛选下拉的全量去重选项(模型/分组)。
func (s *Server) adminLogOptions(w http.ResponseWriter, r *http.Request) {
	opt, err := s.store.LogFilterOptions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, opt)
}

// adminSettings GET 返回 / PUT 保存运行时设置。
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		logRetention, logRetentionSource := intSettingValue(s.store.GetSetting("request_retention_days", ""), 7)
		alertWebhook, alertWebhookSource := stringSettingValue(s.store.GetSetting("alert_webhook", ""), "")
		alertDebounce, alertDebounceSource := settingValue(s.store.GetSetting("alert_debounce", ""), "60s")
		firstResponseTimeout, firstResponseTimeoutSource := intSettingValue(s.store.GetSetting("first_response_timeout_ms", ""), 120000)
		routeSmart := s.store.GetSetting("route_smart", "on") // 默认开
		writeJSON(w, map[string]string{
			"log_retention":                       s.store.GetSetting("request_retention_days", ""),
			"alert_webhook":                       s.store.GetSetting("alert_webhook", ""),
			"alert_debounce":                      s.store.GetSetting("alert_debounce", ""),
			"first_response_timeout_ms":           s.store.GetSetting("first_response_timeout_ms", ""),
			"route_smart":                         routeSmart,
			"effective_log_retention":             logRetention,
			"effective_alert_webhook":             alertWebhook,
			"effective_alert_debounce":            alertDebounce,
			"effective_first_response_timeout_ms": firstResponseTimeout,
			"log_retention_source":                logRetentionSource,
			"alert_webhook_source":                alertWebhookSource,
			"alert_debounce_source":               alertDebounceSource,
			"first_response_timeout_ms_source":    firstResponseTimeoutSource,
		})
	case http.MethodPut:
		var raw struct {
			LogRetention           any `json:"log_retention"`
			AlertWebhook           any `json:"alert_webhook"`
			AlertDebounce          any `json:"alert_debounce"`
			FirstResponseTimeoutMs any `json:"first_response_timeout_ms"`
			RouteSmart             any `json:"route_smart"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, "设置参数格式无效", 400)
			return
		}
		d := struct {
			LogRetention           string
			AlertWebhook           string
			AlertDebounce          string
			FirstResponseTimeoutMs string
			RouteSmart             string
		}{
			LogRetention:           settingString(raw.LogRetention),
			AlertWebhook:           settingString(raw.AlertWebhook),
			AlertDebounce:          settingString(raw.AlertDebounce),
			FirstResponseTimeoutMs: settingString(raw.FirstResponseTimeoutMs),
			RouteSmart:             settingString(raw.RouteSmart),
		}
		if n, err := strconv.Atoi(d.LogRetention); err != nil || n < 1 || n > 365 {
			http.Error(w, "请求记录保留天数须为 1~365 的整数", 400)
			return
		}
		// 告警 webhook 可空(空=关闭)；非空须 http(s):// 前缀
		if d.AlertWebhook != "" && !strings.HasPrefix(d.AlertWebhook, "http://") && !strings.HasPrefix(d.AlertWebhook, "https://") {
			http.Error(w, "告警 Webhook 须以 http:// 或 https:// 开头，或留空关闭", 400)
			return
		}
		if _, err := time.ParseDuration(d.AlertDebounce); err != nil {
			http.Error(w, "无效去抖间隔，用 30s/1m 格式", 400)
			return
		}
		if n, err := strconv.Atoi(d.FirstResponseTimeoutMs); err != nil || n < 1000 || n > 600000 {
			http.Error(w, "首响应超时须为 1000~600000 的毫秒数(1~600 秒)", 400)
			return
		}
		if d.RouteSmart != "on" && d.RouteSmart != "off" {
			http.Error(w, "智能路由开关须为 on 或 off", 400)
			return
		}
		s.store.SetSetting("request_retention_days", d.LogRetention)
		s.store.SetSetting("alert_webhook", d.AlertWebhook)
		s.store.SetSetting("alert_debounce", d.AlertDebounce)
		s.store.SetSetting("first_response_timeout_ms", d.FirstResponseTimeoutMs)
		s.store.SetSetting("route_smart", d.RouteSmart)
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func settingString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	default:
		return ""
	}
}

func settingValue(dbValue, def string) (string, string) {
	if d, err := time.ParseDuration(dbValue); err == nil && d > 0 {
		return dbValue, "settings"
	}
	return def, "default"
}

func stringSettingValue(dbValue, def string) (string, string) {
	if dbValue != "" {
		return dbValue, "settings"
	}
	return def, "default"
}

func intSettingValue(dbValue string, def int) (string, string) {
	if n, err := strconv.Atoi(dbValue); err == nil && n > 0 {
		return dbValue, "settings"
	}
	return strconv.Itoa(def), "default"
}

// healthView 运行时健康精简视图（成员/上游列表用，不含趋势数组，省带宽）。
type healthView struct {
	State     string  `json:"state"`      // CLOSED 正常 / OPEN 熔断 / HALF_OPEN 半开
	Fails     int     `json:"fails"`      // 当前连续失败数
	Reqs      int64   `json:"reqs"`       // 业务请求总数
	SuccRate  float64 `json:"succ_rate"`  // 成功率 0..1（无请求为 0）
	AvgLatMs  int64   `json:"avg_lat_ms"` // 成功请求平均延迟
	LastProbe int64   `json:"last_probe"` // 最后探测 unix 秒，0=从未探测
}

// toHealthView 把上游级快照转精简视图。state 由调用方传入「聚合后的对外状态」
// (health.EffectiveState)，而非上游级原始 State——这样「单模型上游唯一模型熔断」
// 等情形能如实显示熔断，避免运行时列与真实选路口径(IsAvailable 看模型级)错位。
func toHealthView(sn health.Snapshot, state string) healthView {
	var lastProbe int64
	if !sn.LastProbe.IsZero() {
		lastProbe = sn.LastProbe.Unix()
	}
	return healthView{
		State: state, Fails: sn.Fails, Reqs: sn.Reqs,
		SuccRate: sn.SuccRate, AvgLatMs: sn.AvgLatMs, LastProbe: lastProbe,
	}
}

// modelHealthView 模型级健康精简视图（上游/成员列表展开模型徽章用）。
type modelHealthView struct {
	Model     string  `json:"model"`
	State     string  `json:"state"`
	Fails     int     `json:"fails"`
	LatencyMs int64   `json:"latency_ms"`
	LastProbe int64   `json:"last_probe"`
	LatEWMA   float64 `json:"lat_ewma"`  // 选路用成功延迟 EWMA(ms)，徽章 hover 展示
	SuccEWMA  float64 `json:"succ_ewma"` // 选路用成功率 EWMA(0..1)
}

// toModelHealthViews exposes temporary model capability exclusions.
func toModelHealthViews(ms []health.ModelHealth) []modelHealthView {
	if len(ms) == 0 {
		return nil
	}
	out := make([]modelHealthView, 0, len(ms))
	for _, mh := range ms {
		out = append(out, modelHealthView{
			Model: mh.Model, State: mh.State, Fails: mh.Fails,
			LatencyMs: mh.LatencyMs, LastProbe: mh.LastProbe,
			LatEWMA: mh.LatEWMA, SuccEWMA: mh.SuccEWMA,
		})
	}
	return out
}

// effectivePriority 返回分组「生效层」的优先级值。
// 生效层 = enabled 且未熔断(CLOSED/HALF_OPEN 都算可用，与调度 IsAvailable 一致)中优先级最小的那层。
func effectivePriority(ms []*store.Member, state func(int64) string) (int, bool) {
	best, ok := 0, false
	for _, m := range ms {
		if !m.Enabled || !m.GroupEnabled || state(m.UpstreamID) == "OPEN" {
			continue
		}
		if !ok || m.Priority < best {
			best, ok = m.Priority, true
		}
	}
	return best, ok
}

// memberOut 组成员 + 运行时健康 + 是否生效层。
type memberOut struct {
	*store.Member
	Health      healthView        `json:"health"`
	ModelHealth []modelHealthView `json:"model_health,omitempty"` // 该上游模型级健康（仅 GET 填充，无则省略）
	Effective   bool              `json:"effective"`
	// 标准 P2C 的渠道级流量分配预估。
	RoutePreview []routeShareView `json:"route_preview,omitempty"`
}

// routeShareView is a channel-level P2C share preview.
type routeShareView struct {
	Model        string  `json:"model"`
	LatEWMAMs    float64 `json:"lat_ewma_ms"`    // 选路用成功延迟 EWMA(ms)
	SuccRate     float64 `json:"succ_rate"`      // 选路用成功率(0..1)
	EffLatencyMs float64 `json:"eff_latency_ms"` // P2C 比较延迟(ms)
	SharePct     float64 `json:"share_pct"`      // 预估流量占比 0..100
}

// groupRuntime 分组运行时概览：生效渠道名 + 各健康档计数（只统计 enabled 成员）。
type groupRuntime struct {
	Effective []string `json:"effective"` // 生效层渠道名（同层多个全列）
	Normal    int      `json:"normal"`    // CLOSED
	HalfOpen  int      `json:"half_open"` // HALF_OPEN
	Open      int      `json:"open"`      // OPEN 熔断
	Total     int      `json:"total"`     // enabled 成员总数
}

// groupOut 分组 + 运行时概览。
type groupOut struct {
	*store.Group
	Runtime groupRuntime `json:"runtime"`
}

// computeGroupRuntime 算一个分组的运行时概览。
func (s *Server) computeGroupRuntime(gid int64) groupRuntime {
	ms, _ := s.store.ListGroupMembers(gid)
	// 缓存各成员「聚合后对外状态」（含模型级聚合），避免重复加锁查询
	eff := make(map[int64]string, len(ms))
	for _, m := range ms {
		eff[m.UpstreamID] = s.health.EffectiveState(m.UpstreamID)
	}
	rt := groupRuntime{Effective: []string{}}
	for _, m := range ms {
		if !m.Enabled || !m.GroupEnabled {
			continue
		}
		rt.Total++
		switch eff[m.UpstreamID] {
		case "OPEN":
			rt.Open++
		case "HALF_OPEN":
			rt.HalfOpen++
		default:
			rt.Normal++
		}
	}
	best, hasEff := effectivePriority(ms, func(id int64) string { return eff[id] })
	if hasEff {
		for _, m := range ms {
			if m.Enabled && m.GroupEnabled && m.Priority == best && eff[m.UpstreamID] != "OPEN" {
				rt.Effective = append(rt.Effective, m.Name)
			}
		}
	}
	return rt
}

// computeRoutePreviews calculates channel-level standard P2C shares for the
// active priority tier. Model capability exclusions do not create breakers.
func (s *Server) computeRoutePreviews(eff []*store.Member) map[int64][]routeShareView {
	out := map[int64][]routeShareView{}
	if len(eff) < 1 {
		return out
	}
	tier := make([]*upstream.Upstream, 0, len(eff))
	for _, member := range eff {
		tier = append(tier, &upstream.Upstream{ID: member.UpstreamID, Weight: member.Weight})
	}
	shares := scheduler.PreviewShares(tier,
		func(id int64) (float64, float64) { return s.health.RouteStats(id, "") }, 0)
	for _, member := range eff {
		ewma, sr := s.health.RouteStats(member.UpstreamID, "")
		share := shares[member.UpstreamID]
		out[member.UpstreamID] = []routeShareView{{
			Model: "渠道级", LatEWMAMs: ewma, SuccRate: sr,
			EffLatencyMs: share.EffLatencyMs, SharePct: share.Share * 100,
		}}
	}
	return out
}

// upstreamDTO 对外视图：api_key 脱敏，不回显完整凭证。
type upstreamDTO struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Source       string            `json:"source"`
	PrimaryTagID int64             `json:"primary_tag_id"`
	TagIDs       []int64           `json:"tag_ids"`
	Tags         []upstream.Tag    `json:"tags"`
	BaseURL      string            `json:"base_url"`
	Proxy        string            `json:"proxy"`
	Protocol     string            `json:"protocol"`
	APIKey       string            `json:"api_key,omitempty"` // 输入用；输出时脱敏到 masked
	Masked       string            `json:"masked,omitempty"`
	Enabled      bool              `json:"enabled"`
	ChannelProbe bool              `json:"channel_probe"`          // 兼容旧数据；熔断固定为渠道级
	Health       healthView        `json:"health"`                 // 运行时健康（仅 GET 列表填充）
	ModelHealth  []modelHealthView `json:"model_health,omitempty"` // 模型级健康（仅 GET 列表填充，无则省略）
}

func mask(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

func (s *Server) adminUpstreams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.List()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := make([]upstreamDTO, 0, len(list))
		for _, u := range list {
			out = append(out, upstreamDTO{
				ID: u.ID, Name: u.Name, Source: u.Source, BaseURL: u.BaseURL, Proxy: u.Proxy, Protocol: u.Protocol,
				PrimaryTagID: u.PrimaryTagID, TagIDs: u.TagIDs, Tags: u.Tags,
				Masked: mask(u.APIKey), Enabled: u.Enabled, ChannelProbe: u.ChannelProbe,
				Health:      toHealthView(s.health.Snapshot(u.ID), s.health.EffectiveState(u.ID)),
				ModelHealth: toModelHealthViews(s.health.ModelStates(u.ID)),
			})
		}
		writeJSON(w, out)
	case http.MethodPost:
		u, err := decodeUpstream(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.Create(u); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) adminUpstreamItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/upstreams/")
	parts := strings.Split(rest, "/")
	if parts[0] == "batch" && r.Method == http.MethodPost {
		s.batchUpdateUpstreams(w, r)
		return
	}
	if parts[0] == "reorder" && r.Method == http.MethodPost {
		s.reorderUpstreams(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if len(parts) == 2 && parts[1] == "models" { // 连通测试 + 拉模型
		s.testUpstream(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "test" { // 真实对话测试(SSE流式回显)
		s.testUpstreamChat(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "monitors" && r.Method == http.MethodPost { // 批量建监控
		s.batchCreateMonitors(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodPut:
		u, err := decodeUpstream(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		u.ID = id
		if err := s.store.Update(u); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodDelete:
		if err := s.store.Delete(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) batchUpdateUpstreams(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs          []int64 `json:"ids"`
		Enabled      *bool   `json:"enabled"`
		PrimaryTagID *int64  `json:"primary_tag_id"`
		AddTagIDs    []int64 `json:"add_tag_ids"`
		RemoveTagIDs []int64 `json:"remove_tag_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	update := store.UpstreamBatchUpdate{
		Enabled: in.Enabled, PrimaryTagID: in.PrimaryTagID,
		AddTagIDs: in.AddTagIDs, RemoveTagIDs: in.RemoveTagIDs,
	}
	if err := s.store.BatchUpdateUpstreams(in.IDs, update); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var tagColors = map[string]bool{
	"gray": true, "green": true, "amber": true, "red": true,
	"blue": true, "purple": true, "pink": true,
}

func decodeTag(r *http.Request) (string, string, error) {
	var in struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return "", "", err
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Color = strings.TrimSpace(in.Color)
	if in.Name == "" || len([]rune(in.Name)) > 40 {
		return "", "", errors.New("tag name must be 1-40 characters")
	}
	if !tagColors[in.Color] {
		return "", "", errors.New("unsupported tag color")
	}
	return in.Name, in.Color, nil
}

func (s *Server) adminTags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tags, err := s.store.ListTags()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, tags)
	case http.MethodPost:
		name, color, err := decodeTag(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		id, err := s.store.CreateTag(name, color)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]int64{"id": id})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) adminTagItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/admin/tags/"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", 400)
		return
	}
	switch r.Method {
	case http.MethodPut:
		name, color, err := decodeTag(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.UpdateTag(id, name, color); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := s.store.DeleteTag(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// testUpstream 实时拉该上游 /v1/models：既是连通测试，也返回模型列表。
func (s *Server) testUpstream(w http.ResponseWriter, r *http.Request, id int64) {
	u, err := s.store.Get(id)
	if err != nil {
		http.Error(w, "upstream not found", 404)
		return
	}
	type result struct {
		OK        bool     `json:"ok"`
		Status    int      `json:"status,omitempty"`
		LatencyMs int64    `json:"latency_ms"`
		Models    []string `json:"models,omitempty"`
		Error     string   `json:"error,omitempty"`
	}
	start := time.Now()
	models, status, err := u.FetchModels(10 * time.Second)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, result{Status: status, LatencyMs: lat, Error: err.Error()})
		return
	}
	writeJSON(w, result{OK: true, Status: status, LatencyMs: lat, Models: models})
}

// batchCreateMonitors 为某上游的一批模型批量建监控，已存在的跳过。
// body: {models:[], stream, probe_text, max_tokens, interval_sec, path, enabled}
func (s *Server) batchCreateMonitors(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		Models      []string `json:"models"`
		Enabled     bool     `json:"enabled"`
		Stream      bool     `json:"stream"`
		ProbeText   string   `json:"probe_text"`
		MaxTokens   int      `json:"max_tokens"`
		IntervalSec int      `json:"interval_sec"`
		Path        string   `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(in.Models) == 0 {
		http.Error(w, "no models selected", 400)
		return
	}
	// 校验 upstream 存在：与单建监控同口径，杜绝孤儿监控行
	if _, err := s.store.Get(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "upstream not found", 404)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	tmpl := store.Monitor{
		Enabled: in.Enabled, Stream: in.Stream, ProbeText: strings.TrimSpace(in.ProbeText),
		MaxTokens: in.MaxTokens, IntervalSec: in.IntervalSec, Path: strings.TrimSpace(in.Path),
	}
	created, skipped, err := s.store.BatchCreateMonitors(id, in.Models, tmpl)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]int{"created": created, "skipped": skipped})
}

type monitorDTO struct {
	ID           int64            `json:"id"`
	UpstreamID   int64            `json:"upstream_id"`
	UpstreamName string           `json:"upstream_name"`
	Model        string           `json:"model"`
	Name         string           `json:"name"`
	Enabled      bool             `json:"enabled"`
	Stream       bool             `json:"stream"`
	ProbeText    string           `json:"probe_text"`
	MaxTokens    int              `json:"max_tokens"`
	IntervalSec  int              `json:"interval_sec"`
	Path         string           `json:"path"`
	Snapshot     monitor.Snapshot `json:"snapshot"`
}

// monitorInput 新增/编辑入参。
type monitorInput struct {
	UpstreamID  int64  `json:"upstream_id"`
	Model       string `json:"model"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Stream      bool   `json:"stream"`
	ProbeText   string `json:"probe_text"`
	MaxTokens   int    `json:"max_tokens"`
	IntervalSec int    `json:"interval_sec"`
	Path        string `json:"path"`
}

func (s *Server) adminMonitors(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ms, err := s.store.ListMonitors(false)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := make([]monitorDTO, 0, len(ms))
		for _, m := range ms {
			out = append(out, monitorDTO{
				ID: m.ID, UpstreamID: m.UpstreamID, UpstreamName: m.UpstreamName,
				Model: m.Model, Name: m.Name, Enabled: m.Enabled,
				Stream: m.Stream, ProbeText: m.ProbeText, MaxTokens: m.MaxTokens,
				IntervalSec: m.IntervalSec, Path: m.Path,
				Snapshot: s.mon.Snapshot(m.ID),
			})
		}
		writeJSON(w, out)
	case http.MethodPost:
		in, err := decodeMonitor(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// 校验 upstream 存在：避免建出孤儿监控行(被 INNER JOIN 过滤但 POST 误返回成功)
		if _, err := s.store.Get(in.UpstreamID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "upstream not found", 404)
				return
			}
			http.Error(w, err.Error(), 500)
			return
		}
		id, err := s.store.CreateMonitor(in)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]int64{"id": id})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// reorderUpstreams 持久化拖拽后的上游顺序。body: {ids:[3,1,2,...]}
func (s *Server) reorderUpstreams(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.ReorderUpstreams(in.IDs); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) adminMonitorItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/monitors/")
	parts := strings.Split(rest, "/")
	if parts[0] == "reorder" && r.Method == http.MethodPost { // 拖拽保存顺序
		s.reorderMonitors(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if len(parts) == 2 && parts[1] == "probe" { // 立即探测一次并返回最新快照
		s.probeMonitorNow(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodPut:
		in, err := decodeMonitor(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		in.ID = id
		if err := s.store.UpdateMonitor(in); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodDelete:
		if err := s.store.DeleteMonitor(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.mon.Forget(id)
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// reorderMonitors 持久化拖拽后的卡片顺序。body: {ids:[3,1,2,...]}
func (s *Server) reorderMonitors(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.ReorderMonitors(in.IDs); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

// probeMonitorNow 立即同步探测该监控项一次，返回最新快照。
func (s *Server) probeMonitorNow(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	m, err := s.store.GetMonitor(id)
	if err != nil || m == nil {
		http.Error(w, "monitor not found", 404)
		return
	}
	s.monProber.Probe(r.Context(), m)
	writeJSON(w, s.mon.Snapshot(id))
}

func decodeMonitor(r *http.Request) (*store.Monitor, error) {
	var in monitorInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, err
	}
	if in.UpstreamID <= 0 || strings.TrimSpace(in.Model) == "" {
		return nil, errBadMonitor
	}
	if in.MaxTokens < 0 {
		in.MaxTokens = 0
	}
	if in.IntervalSec < 0 {
		in.IntervalSec = 0
	}
	return &store.Monitor{
		UpstreamID: in.UpstreamID, Model: strings.TrimSpace(in.Model),
		Name: strings.TrimSpace(in.Name), Enabled: in.Enabled,
		Stream: in.Stream, ProbeText: in.ProbeText, MaxTokens: in.MaxTokens,
		IntervalSec: in.IntervalSec, Path: strings.TrimSpace(in.Path),
	}, nil
}

type upstreamInput struct {
	Name         string   `json:"name"`
	Source       string   `json:"source"`
	BaseURL      string   `json:"base_url"`
	Proxy        string   `json:"proxy"`
	Protocol     string   `json:"protocol"`
	APIKey       string   `json:"api_key"`
	Enabled      bool     `json:"enabled"`
	ChannelProbe bool     `json:"channel_probe"`
	PrimaryTagID *int64   `json:"primary_tag_id"`
	TagIDs       *[]int64 `json:"tag_ids"`
}

func decodeUpstream(r *http.Request) (*upstream.Upstream, error) {
	var d upstreamInput
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		return nil, err
	}
	d.Name = strings.TrimSpace(d.Name)
	d.Source = strings.TrimSpace(d.Source)
	d.BaseURL = strings.TrimSpace(d.BaseURL)
	d.Protocol = strings.TrimSpace(d.Protocol)
	if d.Name == "" {
		return nil, errors.New("name is required")
	}
	// BaseURL 必须是 http/https 绝对 URL（系统边界严格校验）
	u, err := url.Parse(d.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("base_url must be a valid http(s) URL")
	}
	protocol, ok := translate.NormalizeFormat(d.Protocol)
	if !ok {
		return nil, errors.New("unsupported upstream protocol")
	}
	result := &upstream.Upstream{
		Name: d.Name, Source: d.Source, BaseURL: d.BaseURL, APIKey: d.APIKey, Proxy: d.Proxy, Enabled: d.Enabled,
		Protocol: string(protocol), ChannelProbe: d.ChannelProbe,
	}
	if d.PrimaryTagID != nil || d.TagIDs != nil {
		result.TagsSet = true
		if d.PrimaryTagID != nil && *d.PrimaryTagID > 0 {
			result.PrimaryTagID = *d.PrimaryTagID
		}
		if d.TagIDs != nil {
			result.TagIDs = *d.TagIDs
		}
	}
	return result, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// --- 分组 ---
func (s *Server) adminGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		gs, err := s.store.ListGroups()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := make([]groupOut, 0, len(gs))
		for _, g := range gs {
			out = append(out, groupOut{Group: g, Runtime: s.computeGroupRuntime(g.ID)})
		}
		writeJSON(w, out)
	case http.MethodPost:
		var d struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if json.NewDecoder(r.Body).Decode(&d) != nil || d.Name == "" {
			http.Error(w, "bad name", 400)
			return
		}
		if _, err := s.store.CreateGroup(d.Name, d.Description); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// adminGroupSub 路由 /admin/groups/{id}、/{id}/upstreams、/{id}/upstreams/{uid}、/{id}/keys。
func (s *Server) adminGroupSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/groups/")
	parts := strings.Split(rest, "/")
	gid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad group id", 400)
		return
	}
	switch {
	case len(parts) == 1: // /{id}
		switch r.Method {
		case http.MethodPut:
			var d struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if json.NewDecoder(r.Body).Decode(&d) != nil || d.Name == "" {
				http.Error(w, "bad name", 400)
				return
			}
			if err := s.store.UpdateGroup(gid, d.Name, d.Description); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		case http.MethodDelete:
			if err := s.store.DeleteGroup(gid); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		default:
			http.Error(w, "method not allowed", 405)
			return
		}
	case parts[1] == "upstreams":
		s.adminGroupUpstreams(w, r, gid, parts)
	case parts[1] == "keys":
		s.adminGroupKeys(w, r, gid)
	default:
		http.Error(w, "not found", 404)
	}
}

// adminGroupUpstreams 组成员：GET 列表 / POST 加入(带组内prio/weight) /
// PUT 启停(/upstreams/{uid} 带 {enabled}) / DELETE 移除(/upstreams/{uid})。
func (s *Server) adminGroupUpstreams(w http.ResponseWriter, r *http.Request, gid int64, parts []string) {
	if len(parts) == 3 { // /{id}/upstreams/{uid}
		uid, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.Error(w, "bad upstream id", 400)
			return
		}
		switch r.Method {
		case http.MethodPut: // 组内启停
			var d struct {
				Enabled bool `json:"enabled"`
			}
			if json.NewDecoder(r.Body).Decode(&d) != nil {
				http.Error(w, "bad request body", 400)
				return
			}
			if err := s.store.SetMemberEnabled(gid, uid, d.Enabled); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
			return
		case http.MethodDelete: // 移除成员
			if err := s.store.RemoveMember(gid, uid); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
			return
		default:
			http.Error(w, "method not allowed", 405)
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		ms, err := s.store.ListGroupMembers(gid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// 缓存各成员快照(含 reqs/延迟)与「聚合后对外状态」，避免重复加锁查询
		snaps := make(map[int64]health.Snapshot, len(ms))
		effSt := make(map[int64]string, len(ms))
		for _, m := range ms {
			snaps[m.UpstreamID] = s.health.Snapshot(m.UpstreamID)
			effSt[m.UpstreamID] = s.health.EffectiveState(m.UpstreamID)
		}
		state := func(id int64) string { return effSt[id] }
		best, hasEff := effectivePriority(ms, state)
		// 生效层成员集合 → 算标准 P2C 流量占比预估
		eff := make([]*store.Member, 0)
		for _, m := range ms {
			if hasEff && m.Enabled && m.GroupEnabled && m.Priority == best && effSt[m.UpstreamID] != "OPEN" {
				eff = append(eff, m)
			}
		}
		previews := s.computeRoutePreviews(eff)
		out := make([]memberOut, 0, len(ms))
		for _, m := range ms {
			isEff := hasEff && m.Enabled && m.GroupEnabled && m.Priority == best && effSt[m.UpstreamID] != "OPEN"
			out = append(out, memberOut{
				Member:       m,
				Health:       toHealthView(snaps[m.UpstreamID], effSt[m.UpstreamID]),
				ModelHealth:  toModelHealthViews(s.health.ModelStates(m.UpstreamID)),
				Effective:    isEff,
				RoutePreview: previews[m.UpstreamID],
			})
		}
		writeJSON(w, out)
	case http.MethodPost, http.MethodPut: // 加入或更新组内策略（UPSERT）
		var d struct {
			UpstreamID int64 `json:"upstream_id"`
			Priority   int   `json:"priority"`
			Weight     int   `json:"weight"`
		}
		if json.NewDecoder(r.Body).Decode(&d) != nil || d.UpstreamID == 0 {
			http.Error(w, "bad upstream_id", 400)
			return
		}
		if err := s.store.AddMember(gid, d.UpstreamID, d.Priority, d.Weight); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// --- 接入 key（归口到分组）---
func (s *Server) adminGroupKeys(w http.ResponseWriter, r *http.Request, gid int64) {
	switch r.Method {
	case http.MethodGet:
		ks, err := s.store.ListKeys(gid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type item struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Key     string `json:"key"`
			Masked  string `json:"masked"`
			Enabled bool   `json:"enabled"`
		}
		out := make([]item, 0, len(ks))
		for _, k := range ks {
			out = append(out, item{k.ID, k.Name, k.Key, mask(k.Key), k.Enabled})
		}
		writeJSON(w, out)
	case http.MethodPost: // 系统生成，返回明文(仅此一次)
		var d struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&d)
		key, err := s.store.CreateKey(d.Name, gid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		writeJSON(w, map[string]string{"key": key})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) adminKeyItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/admin/keys/"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	switch r.Method {
	case http.MethodPut: // 启停
		var d struct {
			Enabled bool `json:"enabled"`
		}
		if json.NewDecoder(r.Body).Decode(&d) != nil {
			http.Error(w, "bad request body", 400)
			return
		}
		if err := s.store.SetKeyEnabled(id, d.Enabled); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodDelete:
		if err := s.store.DeleteKey(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}
