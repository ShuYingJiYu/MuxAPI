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

	"github.com/mirainya/muxapi/internal/store"
)

var errBadMonitor = errors.New("monitor requires upstream_id and model")

// registerAdmin 集中注册管理接口，并统一套用管理员鉴权。
func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin/upstreams", s.auth(s.adminUpstreams))            // GET 全局池 / POST 新增
	mux.HandleFunc("/admin/upstreams/", s.auth(s.adminUpstreamItem))        // PUT 改 / DELETE 删
	mux.HandleFunc("/admin/tags", s.auth(s.adminTags))                      // GET 列表 / POST 新增
	mux.HandleFunc("/admin/tags/", s.auth(s.adminTagItem))                  // PUT 改 / DELETE 删
	mux.HandleFunc("/admin/monitors", s.auth(s.adminMonitors))              // GET 监控列表 / POST 新增
	mux.HandleFunc("/admin/monitors/", s.auth(s.adminMonitorItem))          // PUT 改 / DELETE 删 / {id}/probe 立即探测
	mux.HandleFunc("/admin/groups", s.auth(s.adminGroups))                  // GET 列表 / POST 新增
	mux.HandleFunc("/admin/groups/", s.auth(s.adminGroupSub))               // /{id} 改/删 ; /{id}/upstreams 成员 ; /{id}/keys 密钥
	mux.HandleFunc("/admin/keys/", s.auth(s.adminKeyItem))                  // PUT 启停 / DELETE 删
	mux.HandleFunc("/admin/logs", s.auth(s.adminLogs))                      // GET 调用日志(游标分页+筛选)
	mux.HandleFunc("/admin/logs/stats", s.auth(s.adminLogStats))            // GET 当前筛选范围统计
	mux.HandleFunc("/admin/logs/cache-stats", s.auth(s.adminLogCacheStats)) // GET 按渠道汇总缓存命中率
	mux.HandleFunc("/admin/logs/options", s.auth(s.adminLogOptions))        // GET 筛选下拉选项(全量去重)
	mux.HandleFunc("/admin/logs/", s.auth(s.adminLogItem))                  // GET 单条请求完整尝试链
	mux.HandleFunc("/admin/settings", s.auth(s.adminSettings))              // GET/PUT 运行时设置
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

// parseLogFilter 将查询参数转换为存储层筛选条件，并限制分页大小。
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

func (s *Server) adminLogCacheStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.RequestCacheStats(parseLogFilter(r.URL.Query()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		writeJSON(w, map[string]string{
			"log_retention":                       s.store.GetSetting("request_retention_days", ""),
			"alert_webhook":                       s.store.GetSetting("alert_webhook", ""),
			"alert_debounce":                      s.store.GetSetting("alert_debounce", ""),
			"first_response_timeout_ms":           s.store.GetSetting("first_response_timeout_ms", ""),
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
		}{
			LogRetention:           settingString(raw.LogRetention),
			AlertWebhook:           settingString(raw.AlertWebhook),
			AlertDebounce:          settingString(raw.AlertDebounce),
			FirstResponseTimeoutMs: settingString(raw.FirstResponseTimeoutMs),
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
		s.store.SetSetting("request_retention_days", d.LogRetention)
		s.store.SetSetting("alert_webhook", d.AlertWebhook)
		s.store.SetSetting("alert_debounce", d.AlertDebounce)
		s.store.SetSetting("first_response_timeout_ms", d.FirstResponseTimeoutMs)
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
