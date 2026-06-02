package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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
		probeValue, probeSource := settingValue(s.store.GetSetting("probe_interval", ""), "15s")
		monitorValue, monitorSource := settingValue(s.store.GetSetting("monitor_interval", ""), "5m")
		probeModel, probeModelSource := stringSettingValue(s.store.GetSetting("probe_model", ""), "gpt-4o-mini")
		probePath, probePathSource := stringSettingValue(s.store.GetSetting("probe_path", ""), "/v1/chat/completions")
		writeJSON(w, map[string]string{
			"probe_interval":             s.store.GetSetting("probe_interval", ""),
			"monitor_interval":           s.store.GetSetting("monitor_interval", ""),
			"probe_model":                s.store.GetSetting("probe_model", ""),
			"probe_path":                 s.store.GetSetting("probe_path", ""),
			"effective_probe_interval":   probeValue,
			"effective_monitor_interval": monitorValue,
			"effective_probe_model":      probeModel,
			"effective_probe_path":       probePath,
			"probe_source":               probeSource,
			"monitor_source":             monitorSource,
			"probe_model_source":         probeModelSource,
			"probe_path_source":          probePathSource,
		})
	case http.MethodPut:
		var d struct {
			ProbeInterval   string `json:"probe_interval"`
			MonitorInterval string `json:"monitor_interval"`
			ProbeModel      string `json:"probe_model"`
			ProbePath       string `json:"probe_path"`
		}
		json.NewDecoder(r.Body).Decode(&d)
		for _, v := range []string{d.ProbeInterval, d.MonitorInterval} {
			if _, err := time.ParseDuration(v); err != nil {
				http.Error(w, "无效间隔，用 30s/2m/1h 格式", 400)
				return
			}
		}
		if d.ProbeModel == "" || d.ProbePath == "" || !strings.HasPrefix(d.ProbePath, "/") {
			http.Error(w, "探测模型不能为空，探测路径必须以 / 开头", 400)
			return
		}
		s.store.SetSetting("probe_interval", d.ProbeInterval)
		s.store.SetSetting("monitor_interval", d.MonitorInterval)
		s.store.SetSetting("probe_model", d.ProbeModel)
		s.store.SetSetting("probe_path", d.ProbePath)
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

// upstreamDTO 对外视图：api_key 脱敏，不回显完整凭证。
type upstreamDTO struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Proxy   string `json:"proxy"`
	APIKey  string `json:"api_key,omitempty"` // 输入用；输出时脱敏到 masked
	Masked  string `json:"masked,omitempty"`
	Enabled bool   `json:"enabled"`
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
			out = append(out, upstreamDTO{u.ID, u.Name, u.BaseURL, u.Proxy, "", mask(u.APIKey), u.Enabled})
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
	req, err := u.BuildRequest(http.MethodGet, "/v1/models", nil, http.Header{})
	if err != nil {
		writeJSON(w, result{Error: err.Error()})
		return
	}
	start := time.Now()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, result{LatencyMs: lat, Error: err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		writeJSON(w, result{Status: resp.StatusCode, LatencyMs: lat, Error: strings.TrimSpace(string(body))})
		return
	}
	// 解析 OpenAI 风格 {"data":[{"id":...}]}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(body, &parsed)
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		models = append(models, m.ID)
	}
	writeJSON(w, result{OK: true, Status: resp.StatusCode, LatencyMs: lat, Models: models})
}

// monitorDTO 监控项对外视图：配置字段 + 实时探测快照(展平)。
type monitorDTO struct {
	ID           int64            `json:"id"`
	UpstreamID   int64            `json:"upstream_id"`
	UpstreamName string           `json:"upstream_name"`
	Model        string           `json:"model"`
	Name         string           `json:"name"`
	Enabled      bool             `json:"enabled"`
	Snapshot     monitor.Snapshot `json:"snapshot"`
}

// monitorInput 新增/编辑入参。
type monitorInput struct {
	UpstreamID int64  `json:"upstream_id"`
	Model      string `json:"model"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
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
	return &store.Monitor{
		UpstreamID: in.UpstreamID, Model: strings.TrimSpace(in.Model),
		Name: strings.TrimSpace(in.Name), Enabled: in.Enabled,
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
		writeJSON(w, gs)
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

// adminGroupUpstreams 组成员：GET 列表 / POST 加入(带组内prio/weight) / DELETE 移除(/upstreams/{uid})。
func (s *Server) adminGroupUpstreams(w http.ResponseWriter, r *http.Request, gid int64, parts []string) {
	if len(parts) == 3 && r.Method == http.MethodDelete { // /{id}/upstreams/{uid}
		uid, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.Error(w, "bad upstream id", 400)
			return
		}
		if err := s.store.RemoveMember(gid, uid); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
		return
	}
	switch r.Method {
	case http.MethodGet:
		ms, err := s.store.ListGroupMembers(gid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, ms)
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
