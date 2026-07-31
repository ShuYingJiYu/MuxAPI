package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultFailThreshold = 3
	defaultCooldown      = "30s"
	defaultMaxAttempts   = 6
	defaultMaxBodyBytes  = 32 << 20
)

// adminSettings reads and updates database-backed runtime settings.
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getRuntimeSettings(w)
	case http.MethodPut:
		s.putRuntimeSettings(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getRuntimeSettings(w http.ResponseWriter) {
	logRetention, logRetentionSource := intSettingValue(s.store.GetSetting("request_retention_days", ""), 7)
	alertWebhook, alertWebhookSource := stringSettingValue(s.store.GetSetting("alert_webhook", ""), "")
	alertDebounce, alertDebounceSource := settingValue(s.store.GetSetting("alert_debounce", ""), "60s")
	firstResponseTimeout, firstResponseTimeoutSource := intSettingValue(s.store.GetSetting("first_response_timeout_ms", ""), 120000)
	failThreshold, failThresholdSource := intSettingValue(s.store.GetSetting("fail_threshold", ""), defaultFailThreshold)
	cooldown, cooldownSource := settingValue(s.store.GetSetting("cooldown", ""), defaultCooldown)
	maxAttempts, maxAttemptsSource := intSettingValue(s.store.GetSetting("max_upstream_attempts", ""), defaultMaxAttempts)
	maxBodyBytes, maxBodySource := intSettingValue(s.store.GetSetting("max_body_bytes", ""), defaultMaxBodyBytes)

	writeJSON(w, map[string]string{
		"log_retention":                       s.store.GetSetting("request_retention_days", ""),
		"alert_webhook":                       s.store.GetSetting("alert_webhook", ""),
		"alert_debounce":                      s.store.GetSetting("alert_debounce", ""),
		"first_response_timeout_ms":           s.store.GetSetting("first_response_timeout_ms", ""),
		"fail_threshold":                      s.store.GetSetting("fail_threshold", ""),
		"cooldown":                            s.store.GetSetting("cooldown", ""),
		"max_upstream_attempts":               s.store.GetSetting("max_upstream_attempts", ""),
		"max_body_bytes":                      s.store.GetSetting("max_body_bytes", ""),
		"effective_log_retention":             logRetention,
		"effective_alert_webhook":             alertWebhook,
		"effective_alert_debounce":            alertDebounce,
		"effective_first_response_timeout_ms": firstResponseTimeout,
		"effective_fail_threshold":            failThreshold,
		"effective_cooldown":                  cooldown,
		"effective_max_upstream_attempts":     maxAttempts,
		"effective_max_body_bytes":            maxBodyBytes,
		"log_retention_source":                logRetentionSource,
		"alert_webhook_source":                alertWebhookSource,
		"alert_debounce_source":               alertDebounceSource,
		"first_response_timeout_ms_source":    firstResponseTimeoutSource,
		"fail_threshold_source":               failThresholdSource,
		"cooldown_source":                     cooldownSource,
		"max_upstream_attempts_source":        maxAttemptsSource,
		"max_body_bytes_source":               maxBodySource,
	})
}

type runtimeSettingsInput struct {
	LogRetention           any `json:"log_retention"`
	AlertWebhook           any `json:"alert_webhook"`
	AlertDebounce          any `json:"alert_debounce"`
	FirstResponseTimeoutMs any `json:"first_response_timeout_ms"`
	FailThreshold          any `json:"fail_threshold"`
	Cooldown               any `json:"cooldown"`
	MaxUpstreamAttempts    any `json:"max_upstream_attempts"`
	MaxBodyBytes           any `json:"max_body_bytes"`
}

func (s *Server) putRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	var raw runtimeSettingsInput
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "设置参数格式无效", http.StatusBadRequest)
		return
	}

	values := map[string]string{
		"request_retention_days":    settingString(raw.LogRetention),
		"alert_webhook":             settingString(raw.AlertWebhook),
		"alert_debounce":            settingString(raw.AlertDebounce),
		"first_response_timeout_ms": settingString(raw.FirstResponseTimeoutMs),
		"fail_threshold":            settingString(raw.FailThreshold),
		"cooldown":                  settingString(raw.Cooldown),
		"max_upstream_attempts":     settingString(raw.MaxUpstreamAttempts),
		"max_body_bytes":            settingString(raw.MaxBodyBytes),
	}
	provided := map[string]bool{
		"request_retention_days":    raw.LogRetention != nil,
		"alert_webhook":             raw.AlertWebhook != nil,
		"alert_debounce":            raw.AlertDebounce != nil,
		"first_response_timeout_ms": raw.FirstResponseTimeoutMs != nil,
		"fail_threshold":            raw.FailThreshold != nil,
		"cooldown":                  raw.Cooldown != nil,
		"max_upstream_attempts":     raw.MaxUpstreamAttempts != nil,
		"max_body_bytes":            raw.MaxBodyBytes != nil,
	}
	for _, key := range []string{
		"request_retention_days", "alert_debounce", "first_response_timeout_ms",
		"fail_threshold", "cooldown", "max_upstream_attempts", "max_body_bytes",
	} {
		if provided[key] && strings.TrimSpace(values[key]) == "" {
			http.Error(w, "设置项不能为空", http.StatusBadRequest)
			return
		}
	}

	if err := validateRuntimeSettings(values); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updates := make(map[string]string, len(values))
	for key, value := range values {
		if provided[key] {
			updates[key] = value
		}
	}
	if err := s.store.SetSettings(updates); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.settingsChanged != nil {
		s.settingsChanged()
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateRuntimeSettings(values map[string]string) error {
	if value := values["request_retention_days"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 365 {
			return settingsError("请求记录保留天数须为 1~365 的整数")
		}
	}
	webhook := values["alert_webhook"]
	if webhook != "" && !strings.HasPrefix(webhook, "http://") && !strings.HasPrefix(webhook, "https://") {
		return settingsError("告警 Webhook 须以 http:// 或 https:// 开头")
	}
	if value := values["alert_debounce"]; value != "" {
		if _, err := time.ParseDuration(value); err != nil {
			return settingsError("告警间隔格式无效")
		}
	}
	if value := values["first_response_timeout_ms"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 1000 || n > 600000 {
			return settingsError("首响应超时须为 1000~600000 毫秒")
		}
	}
	if value := values["fail_threshold"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 100 {
			return settingsError("熔断失败阈值须为 1~100 的整数")
		}
	}
	if value := values["cooldown"]; value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration < time.Second || duration > 24*time.Hour {
			return settingsError("熔断冷却时间须在 1 秒到 24 小时之间")
		}
	}
	if value := values["max_upstream_attempts"]; value != "" {
		if n, err := strconv.Atoi(value); err != nil || n < 1 || n > 100 {
			return settingsError("最大上游尝试数须为 1~100 的整数")
		}
	}
	if value := values["max_body_bytes"]; value != "" {
		if n, err := strconv.ParseInt(value, 10, 64); err != nil || n < 1 || n > 256<<20 {
			return settingsError("请求体上限须为 1~268435456 字节")
		}
	}
	return nil
}

type settingsError string

func (e settingsError) Error() string { return string(e) }
