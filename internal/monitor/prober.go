package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// breakerReporter 探测结果喂给熔断器的最小接口（由 health.Manager 实现）。
// 定义在 monitor 侧避免 import 环：health 不依赖 monitor，反向注入即可。
type breakerReporter interface {
	ObserveProbe(id int64, model string, ok bool, latencyMs int64)
}

// Prober 监控探测器：按各监控项自带的渠道+模型+探测参数发最小请求。
// 探测系统统一后，它是唯一的主动探测源，一次探测【双写】：
//   - mgr.Record：记看板统计(成功率/延迟/趋势)，429 算「降级」
//   - breaker.ObserveProbe：驱动路由熔断器，按熔断口径 429 算失败
// 每项可配自己的探测周期(interval_sec)、端点(path)、消息内容、max_tokens、是否流式；
// 这些字段为空/0 时回退到内置默认(间隔 5m、路径 /v1/chat/completions)。
type Prober struct {
	mgr      *Manager
	store    *store.Store
	breaker  breakerReporter      // 探测结果同时驱动路由熔断器；nil 则只记看板
	interval func() time.Duration // 内置默认探测间隔；监控项 interval_sec=0 时用它
	path     func() string        // 内置默认探测端点；监控项 path 为空时用它
}

func NewProber(mgr *Manager, st *store.Store, breaker breakerReporter, interval func() time.Duration, path func() string) *Prober {
	if interval == nil {
		interval = func() time.Duration { return 5 * time.Minute }
	}
	if path == nil {
		path = func() string { return "/v1/chat/completions" }
	}
	return &Prober{mgr: mgr, store: st, breaker: breaker, interval: interval, path: path}
}

// effInterval 该监控项的有效探测周期：自带 interval_sec>0 则用它，否则用全局。
func (p *Prober) effInterval(m *store.Monitor) time.Duration {
	if m.IntervalSec > 0 {
		return time.Duration(m.IntervalSec) * time.Second
	}
	return p.interval()
}

// Run 自调度循环：每轮只探测「距上次探测已达自身周期」的监控项，
// 然后睡到下一个最近到期的时刻（全局间隔作心跳上限，1s 作下限防忙转）。
// 新项的 lastProbe 为零值，会在首轮立即探测。
func (p *Prober) Run(ctx context.Context) {
	last := make(map[int64]time.Time) // 各监控项最后探测时刻；Run 单协程访问，无需加锁
	for {
		now := time.Now()
		ms, _ := p.store.ListMonitors(true)
		live := make(map[int64]bool, len(ms))
		next := p.interval() // 下次唤醒最长不超过全局间隔
		for _, m := range ms {
			live[m.ID] = true
			iv := p.effInterval(m)
			if due := now.Sub(last[m.ID]); due >= iv {
				last[m.ID] = now
				go p.Probe(ctx, m)
				if iv < next {
					next = iv
				}
			} else if remain := iv - due; remain < next {
				next = remain
			}
		}
		for id := range last { // 清理已删除项，避免 map 无限增长
			if !live[id] {
				delete(last, id)
			}
		}
		if next < time.Second {
			next = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// buildProbeBody 按监控项配置生成最小探测请求体。
// 空 ProbeText→"hi"，0 MaxTokens→1，Stream 为真时加 stream:true。
// 用 json.Marshal 编码，自定义文本含引号也不会破坏 JSON。
func buildProbeBody(m *store.Monitor) []byte {
	text := m.ProbeText
	if strings.TrimSpace(text) == "" {
		text = "hi"
	}
	maxTok := m.MaxTokens
	if maxTok <= 0 {
		maxTok = 1
	}
	body := map[string]any{
		"model":      m.Model,
		"max_tokens": maxTok,
		"messages":   []map[string]string{{"role": "user", "content": text}},
	}
	if m.Stream {
		body["stream"] = true
	}
	b, _ := json.Marshal(body)
	return b
}

// Probe 探测单个监控项一次：双写看板统计与路由熔断器。
func (p *Prober) Probe(ctx context.Context, m *store.Monitor) {
	path := m.Path
	if strings.TrimSpace(path) == "" {
		path = p.path()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(m.BaseURL, "/")+path, strings.NewReader(string(buildProbeBody(m))))
	if err != nil {
		p.mgr.Record(m.ID, statDown, 0)
		p.observe(m, false, 0, 0) // 构造失败＝该模型不可用
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("x-api-key", m.APIKey)
	client := &http.Client{Timeout: 30 * time.Second, Transport: upstream.ProxyTransport(m.Proxy)}
	start := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(start).Milliseconds()
	if err != nil { // 网络错误：看板记故障 + 熔断器记模型级失败
		p.mgr.Record(m.ID, statDown, 0)
		p.observe(m, false, 0, 0)
		return
	}
	defer resp.Body.Close()
	p.mgr.Record(m.ID, classify(resp.StatusCode), lat)
	p.observe(m, true, resp.StatusCode, lat) // 熔断器口径单独判定(见 observe)
}

// observe 把探测结果按【熔断器口径】喂路由熔断器（与看板 classify 口径分离）：
// 失败判定用 upstream.IsFailureStatus（429 在此算失败，与看板的「降级」不同）；
// 凭证类(401/402/403)按 upstream 级熔断(scope="")、其余仅熔该模型。
// hasResp=false 表示网络/构造错误，直接按模型级失败处理。
func (p *Prober) observe(m *store.Monitor, hasResp bool, code int, lat int64) {
	if p.breaker == nil {
		return
	}
	if !hasResp {
		p.breaker.ObserveProbe(m.UpstreamID, m.Model, false, 0)
		return
	}
	ok := !upstream.IsFailureStatus(code)
	scope := m.Model
	if !ok && upstream.FailIsUpstreamLevel(code) {
		scope = ""
	}
	p.breaker.ObserveProbe(m.UpstreamID, scope, ok, lat)
}

// classify 把上游状态码映射到栅栏档位：2xx 正常 / 429 降级 / 其余故障。
func classify(code int) int {
	switch {
	case code >= 200 && code < 300:
		return statOK
	case code == http.StatusTooManyRequests:
		return statDegraded
	default:
		return statDown
	}
}
