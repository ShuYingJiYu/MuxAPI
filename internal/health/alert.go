package health

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// 本文件将渠道熔断翻转转换为带去抖控制的 Webhook 通知。

// alertPayload 推送给 webhook 的 JSON 载荷：在 AlertEvent 基础上补充上游名。
type alertPayload struct {
	UpstreamID   int64  `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	Model        string `json:"model"` // 触发渠道状态变化的请求/探测模型
	FromState    string `json:"from_state"`
	ToState      string `json:"to_state"`
	Fails        int    `json:"fails"`
	TS           int64  `json:"ts"` // unix 秒
}

// WebhookAlerter 通用 Webhook 告警：熔断翻转时 POST 一条 JSON。
// webhook URL 为空 → 整体关闭；同键去抖窗口内最多发一次，防 half-open 反复横跳刷屏。
// 设计成非阻塞安全：Notify 由 Manager go 出去调用，内部带超时、失败仅 slog.Warn。
type WebhookAlerter struct {
	webhook  func() string        // 运行时读 settings：URL，空=关闭
	debounce func() time.Duration // 运行时读 settings：去抖窗口
	nameOf   func(int64) string   // id→上游名解析，解析不到回退 id 字符串
	client   *http.Client

	mu       sync.Mutex // 仅护去抖表，独立于 Manager.mu，不与熔断锁纠缠
	lastSent map[debounceKey]time.Time
}

// debounceKey is channel-level. The triggering model remains in the payload,
// but must not create independent debounce slots for one upstream breaker.
type debounceKey struct {
	upstreamID int64
	recovered  bool // true=恢复(→CLOSED)，false=熔断(→OPEN)
}

// NewWebhookAlerter 构造告警器。webhook/debounce 为运行时 getter（页面可配）；
// nameOf 可为 nil（回退 id 字符串）。
func NewWebhookAlerter(webhook func() string, debounce func() time.Duration, nameOf func(int64) string) *WebhookAlerter {
	return &WebhookAlerter{
		webhook:  webhook,
		debounce: debounce,
		nameOf:   nameOf,
		client:   &http.Client{Timeout: 5 * time.Second},
		lastSent: make(map[debounceKey]time.Time),
	}
}

// Notify 发送一条告警。空 URL 直接返回；去抖命中直接返回；否则同步 POST（调用方已 go）。
func (a *WebhookAlerter) Notify(ev AlertEvent) {
	url := ""
	if a.webhook != nil {
		url = a.webhook()
	}
	if url == "" { // 未配置 webhook：告警整体关闭，零侵入
		return
	}
	// 去抖键并入方向：恢复(→CLOSED)与熔断各占独立槽，互不吞没(M1)
	if !a.allow(debounceKey{upstreamID: ev.UpstreamID, recovered: ev.ToState == Closed.String()}) {
		return // 去抖窗口内已发过，丢弃
	}

	name := strconv.FormatInt(ev.UpstreamID, 10) // 回退：解析不到名字用 id
	if a.nameOf != nil {
		if n := a.nameOf(ev.UpstreamID); n != "" {
			name = n
		}
	}
	body, _ := json.Marshal(alertPayload{
		UpstreamID: ev.UpstreamID, UpstreamName: name, Model: ev.Model,
		FromState: ev.FromState, ToState: ev.ToState, Fails: ev.Fails, TS: ev.TS,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("alert webhook build failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		slog.Warn("alert webhook send failed", "err", err, "upstream", ev.UpstreamID)
		return
	}
	defer resp.Body.Close()
	slog.Info("alert sent", "upstream", name, "model", ev.Model,
		"from", ev.FromState, "to", ev.ToState, "status", resp.StatusCode)
}

// allow 去抖判定：同键在窗口内只放行一次。放行时记录本次时间。
func (a *WebhookAlerter) allow(k debounceKey) bool {
	win := 60 * time.Second
	if a.debounce != nil {
		if d := a.debounce(); d > 0 {
			win = d
		}
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.lastSent[k]; ok && now.Sub(last) < win {
		return false
	}
	a.lastSent[k] = now
	return true
}
