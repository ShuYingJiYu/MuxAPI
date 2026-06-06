package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

var errBadMonitor = errors.New("monitor requires upstream_id and model")

func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin/upstreams", s.auth(s.adminUpstreams))     // GET 全局池 / POST 新增
	mux.HandleFunc("/admin/upstreams/", s.auth(s.adminUpstreamItem)) // PUT 改 / DELETE 删
	mux.HandleFunc("/admin/monitors", s.auth(s.adminMonitors))       // GET 监控列表 / POST 新增
	mux.HandleFunc("/admin/monitors/", s.auth(s.adminMonitorItem))   // PUT 改 / DELETE 删 / {id}/probe 立即探测
	mux.HandleFunc("/admin/groups", s.auth(s.adminGroups))           // GET 列表 / POST 新增
	mux.HandleFunc("/admin/groups/", s.auth(s.adminGroupSub))        // /{id} 改/删 ; /{id}/upstreams 成员 ; /{id}/keys 密钥
	mux.HandleFunc("/admin/keys/", s.auth(s.adminKeyItem))           // PUT 启停 / DELETE 删
	mux.HandleFunc("/admin/logs", s.auth(s.adminLogs))               // GET 最近调用日志
	mux.HandleFunc("/admin/settings", s.auth(s.adminSettings))       // GET/PUT 运行时设置
}

// adminLogs 返回最近调用日志(?limit=N，默认100)。
func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	logs, err := s.store.ListLogs(limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if logs == nil {
		logs = []*store.LogEntry{}
	}
	writeJSON(w, logs)
}

// adminSettings GET 返回 / PUT 保存运行时设置。
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		logRetention, logRetentionSource := intSettingValue(s.store.GetSetting("log_retention", ""), 10000)
		alertWebhook, alertWebhookSource := stringSettingValue(s.store.GetSetting("alert_webhook", ""), "")
		alertDebounce, alertDebounceSource := settingValue(s.store.GetSetting("alert_debounce", ""), "60s")
		writeJSON(w, map[string]string{
			"log_retention":            s.store.GetSetting("log_retention", ""),
			"alert_webhook":            s.store.GetSetting("alert_webhook", ""),
			"alert_debounce":           s.store.GetSetting("alert_debounce", ""),
			"effective_log_retention":  logRetention,
			"effective_alert_webhook":  alertWebhook,
			"effective_alert_debounce": alertDebounce,
			"log_retention_source":     logRetentionSource,
			"alert_webhook_source":     alertWebhookSource,
			"alert_debounce_source":    alertDebounceSource,
		})
	case http.MethodPut:
		var d struct {
			LogRetention  string `json:"log_retention"`
			AlertWebhook  string `json:"alert_webhook"`
			AlertDebounce string `json:"alert_debounce"`
		}
		json.NewDecoder(r.Body).Decode(&d)
		if n, err := strconv.Atoi(d.LogRetention); err != nil || n < 100 {
			http.Error(w, "日志保留条数须为 >=100 的整数", 400)
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
		s.store.SetSetting("log_retention", d.LogRetention)
		s.store.SetSetting("alert_webhook", d.AlertWebhook)
		s.store.SetSetting("alert_debounce", d.AlertDebounce)
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
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

func toHealthView(sn health.Snapshot) healthView {
	var lastProbe int64
	if !sn.LastProbe.IsZero() {
		lastProbe = sn.LastProbe.Unix()
	}
	return healthView{
		State: sn.State, Fails: sn.Fails, Reqs: sn.Reqs,
		SuccRate: sn.SuccRate, AvgLatMs: sn.AvgLatMs, LastProbe: lastProbe,
	}
}

// modelHealthView 模型级健康精简视图（上游/成员列表展开模型徽章用）。
type modelHealthView struct {
	Model     string `json:"model"`
	State     string `json:"state"`
	Fails     int    `json:"fails"`
	LatencyMs int64  `json:"latency_ms"`
	LastProbe int64  `json:"last_probe"`
}

// toModelHealthViews 把模型级状态拷成对外视图；无模型级键时返回 nil（前端据此不渲染该区块）。
func toModelHealthViews(ms []health.ModelHealth) []modelHealthView {
	if len(ms) == 0 {
		return nil
	}
	out := make([]modelHealthView, 0, len(ms))
	for _, mh := range ms {
		out = append(out, modelHealthView{
			Model: mh.Model, State: mh.State, Fails: mh.Fails,
			LatencyMs: mh.LatencyMs, LastProbe: mh.LastProbe,
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
	snaps := make(map[int64]health.Snapshot, len(ms))
	for _, m := range ms {
		snaps[m.UpstreamID] = s.health.Snapshot(m.UpstreamID)
	}
	rt := groupRuntime{Effective: []string{}}
	for _, m := range ms {
		if !m.Enabled {
			continue
		}
		rt.Total++
		switch snaps[m.UpstreamID].State {
		case "OPEN":
			rt.Open++
		case "HALF_OPEN":
			rt.HalfOpen++
		default:
			rt.Normal++
		}
	}
	best, hasEff := effectivePriority(ms, func(id int64) string { return snaps[id].State })
	if hasEff {
		for _, m := range ms {
			if m.Enabled && m.Priority == best && snaps[m.UpstreamID].State != "OPEN" {
				rt.Effective = append(rt.Effective, m.Name)
			}
		}
	}
	return rt
}

// upstreamDTO 对外视图：api_key 脱敏，不回显完整凭证。
type upstreamDTO struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	BaseURL     string            `json:"base_url"`
	Proxy       string            `json:"proxy"`
	APIKey      string            `json:"api_key,omitempty"` // 输入用；输出时脱敏到 masked
	Masked      string            `json:"masked,omitempty"`
	Enabled     bool              `json:"enabled"`
	Health      healthView        `json:"health"`                 // 运行时健康（仅 GET 列表填充）
	ModelHealth []modelHealthView `json:"model_health,omitempty"` // 模型级健康（仅 GET 列表填充，无则省略）
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
				ID: u.ID, Name: u.Name, BaseURL: u.BaseURL, Proxy: u.Proxy,
				Masked: mask(u.APIKey), Enabled: u.Enabled,
				Health:      toHealthView(s.health.Snapshot(u.ID)),
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

func decodeUpstream(r *http.Request) (*upstream.Upstream, error) {
	var d upstreamDTO
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		return nil, err
	}
	return &upstream.Upstream{
		Name: d.Name, BaseURL: d.BaseURL, APIKey: d.APIKey, Proxy: d.Proxy, Enabled: d.Enabled,
	}, nil
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
			json.NewDecoder(r.Body).Decode(&d)
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
		// 先缓存各成员健康快照，避免重复加锁查询
		snaps := make(map[int64]health.Snapshot, len(ms))
		for _, m := range ms {
			snaps[m.UpstreamID] = s.health.Snapshot(m.UpstreamID)
		}
		state := func(id int64) string { return snaps[id].State }
		best, hasEff := effectivePriority(ms, state)
		out := make([]memberOut, 0, len(ms))
		for _, m := range ms {
			sn := snaps[m.UpstreamID]
			out = append(out, memberOut{
				Member:      m,
				Health:      toHealthView(sn),
				ModelHealth: toModelHealthViews(s.health.ModelStates(m.UpstreamID)),
				Effective:   hasEff && m.Enabled && m.GroupEnabled && m.Priority == best && sn.State != "OPEN",
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
		json.NewDecoder(r.Body).Decode(&d)
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
