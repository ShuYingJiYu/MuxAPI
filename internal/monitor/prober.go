package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// breakerReporter 探测结果喂给熔断器的最小接口（由 health.Manager 实现）。
// 定义在 monitor 侧避免 import 环：health 不依赖 monitor，反向注入即可。
type breakerReporter interface {
	ObserveProbe(id int64, model string, ok bool, latencyMs int64)
}

// capabilityReporter 将明确的模型支持结果写入短期能力缓存。
type capabilityReporter interface {
	MarkModelUnsupported(id int64, model string)
	MarkModelSupported(id int64, model string)
}

// Prober 监控探测器：按各监控项自带的渠道+模型+探测参数发最小请求。
// 探测系统统一后，它是唯一的主动探测源，一次探测【双写】：
//   - mgr.Record：记看板统计(成功率/延迟/趋势)，429 算「降级」
//   - breaker.ObserveProbe：驱动路由熔断器，按熔断口径 429 算失败
//
// 每项可配自己的探测周期(interval_sec)、端点(path)、消息内容、max_tokens、是否流式；
// 这些字段为空/0 时回退到内置默认(间隔 5m、路径 /v1/chat/completions)。
type Prober struct {
	mgr      *Manager
	store    *store.Store
	breaker  breakerReporter      // 探测结果同时驱动路由熔断器；nil 则只记看板
	interval func() time.Duration // 内置默认探测间隔；监控项 interval_sec=0 时用它
	path     func() string        // 内置默认探测端点；监控项 path 为空时用它
}

// NewProber 创建探测器；nil 默认值会回退到五分钟和 chat completions 端点。
func NewProber(mgr *Manager, st *store.Store, breaker breakerReporter, interval func() time.Duration, path func() string) *Prober {
	if interval == nil {
		interval = func() time.Duration { return 5 * time.Minute }
	}
	if path == nil {
		path = func() string { return "/v1/chat/completions" }
	}
	return &Prober{mgr: mgr, store: st, breaker: breaker, interval: interval, path: path}
}

// minIntervalSec 监控项自带探测周期的下限(秒)：太小会让单项探测尚未返回就被反复重叠派发，
// 徒增上游压力。<此值的自定义 interval_sec 一律抬到此下限（0 仍表示「沿用全局默认」，不受限）。
const minIntervalSec = 5

// effInterval 该监控项的有效探测周期：自带 interval_sec>0 则用它(但不低于下限)，否则用全局。
func (p *Prober) effInterval(m *store.Monitor) time.Duration {
	if m.IntervalSec > 0 {
		sec := m.IntervalSec
		if sec < minIntervalSec {
			sec = minIntervalSec // 下限：防过密探测重叠
		}
		return time.Duration(sec) * time.Second
	}
	return p.interval()
}

// Run 自调度循环：每轮只探测「距上次探测已达自身周期」的监控项，
// 然后睡到下一个最近到期的时刻（全局间隔作心跳上限，1s 作下限防忙转）。
// 新项的 lastProbe 为零值，会在首轮立即探测。
// 在飞标记：同项上轮探测未返回则本轮跳过，避免单次探测超周期时重叠派发压垮上游；
// 探测协程完成后经 done 回传 ID 由本协程清标记（状态仅本协程读写，无需加锁）。
func (p *Prober) Run(ctx context.Context) {
	last := make(map[int64]time.Time) // 各监控项最后探测时刻；Run 单协程访问，无需加锁
	inflight := make(map[int64]bool)  // 正在探测中的项；同上仅本协程读写
	done := make(chan int64, 256)     // 探测协程完成回传其 ID；缓冲足够大，发送侧再带 ctx 兜底防阻塞
	var probes sync.WaitGroup
	for {
		// 先排空已完成回传，清在飞标记（非阻塞，本轮新到期项才能再次派发）
		for {
			select {
			case id := <-done:
				delete(inflight, id)
				continue
			default:
			}
			break
		}
		now := time.Now()
		ms, err := p.store.ListMonitors(true)
		if err != nil {
			slog.Warn("list monitors failed", "err", err)
			select {
			case <-ctx.Done():
				probes.Wait()
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		live := make(map[int64]bool, len(ms))
		next := p.interval() // 下次唤醒最长不超过全局间隔
		for _, m := range ms {
			live[m.ID] = true
			iv := p.effInterval(m)
			due := now.Sub(last[m.ID])
			if due < iv { // 未到期：记下最近的剩余时间作为唤醒上限
				if remain := iv - due; remain < next {
					next = remain
				}
				continue
			}
			if inflight[m.ID] { // 上轮探测仍在飞：跳过，等其完成回传再重新派发
				continue
			}
			last[m.ID] = now
			inflight[m.ID] = true
			m := m // 捕获本轮变量，供探测协程闭包安全引用
			probes.Add(1)
			go func() {
				defer probes.Done()
				p.Probe(ctx, m)
				select {
				case done <- m.ID:
				case <-ctx.Done(): // 退出途中无人消费，丢弃回传防协程泄漏
				}
			}()
			if iv < next {
				next = iv
			}
		}
		for id := range last { // 清理已删除项，避免 map 无限增长
			if !live[id] {
				delete(last, id)
				delete(inflight, id)
			}
		}
		if next < time.Second {
			next = time.Second
		}
		select {
		case <-ctx.Done():
			probes.Wait()
			return
		case id := <-done: // 有探测提前完成则提前醒来，尽快重新评估其是否到期
			delete(inflight, id)
		case <-time.After(next):
		}
	}
}

// buildProbeBody builds a protocol-correct minimal request for each supported
// endpoint instead of sending chat-completions JSON to every protocol.
func buildProbeBody(m *store.Monitor) []byte {
	text := m.ProbeText
	if strings.TrimSpace(text) == "" {
		text = "hi"
	}
	maxTok := m.MaxTokens
	if maxTok <= 0 {
		maxTok = 1
	}
	path := strings.TrimSpace(m.Path)
	var body map[string]any
	switch {
	case strings.HasSuffix(path, "/v1/responses"):
		body = map[string]any{
			"model":             m.Model,
			"input":             text,
			"max_output_tokens": maxTok,
		}
	default:
		body = map[string]any{
			"model":      m.Model,
			"max_tokens": maxTok,
			"messages":   []map[string]string{{"role": "user", "content": text}},
		}
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
		p.observe(m, false, 0, 0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("x-api-key", m.APIKey)
	if strings.HasSuffix(path, "/v1/messages") {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: upstream.ProxyTransport(m.Proxy)}
	start := time.Now()
	resp, err := client.Do(req)
	ttft := time.Since(start).Milliseconds()
	if err != nil { // 网络错误：看板记故障 + 熔断器记模型级失败
		p.mgr.Record(m.ID, statDown, 0)
		p.observe(m, false, 0, 0)
		return
	}
	defer resp.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		p.mgr.Record(m.ID, statDown, 0)
		p.observe(m, false, 0, 0)
		return
	}
	if upstream.IsModelUnsupported(resp.StatusCode, m.Model, string(payload)) {
		p.mgr.Record(m.ID, statDown, ttft)
		if reporter, ok := p.breaker.(capabilityReporter); ok {
			reporter.MarkModelUnsupported(m.UpstreamID, m.Model)
		}
		return
	}
	if upstream.IsFailureStatus(resp.StatusCode) {
		p.mgr.Record(m.ID, classify(resp.StatusCode), ttft)
		p.observe(m, true, resp.StatusCode, ttft)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if validProbePayload(resp.Header.Get("Content-Type"), payload, m.Stream) {
			p.mgr.Record(m.ID, statOK, ttft)
			if reporter, ok := p.breaker.(capabilityReporter); ok {
				reporter.MarkModelSupported(m.UpstreamID, m.Model)
			}
			p.observe(m, true, resp.StatusCode, ttft)
			return
		}
		p.mgr.Record(m.ID, statDown, ttft)
		p.observe(m, false, 0, 0)
		return
	}
	// Other 4xx responses are request/configuration errors. They remain visible
	// on the monitor but do not poison channel health.
	p.mgr.Record(m.ID, classify(resp.StatusCode), ttft)
}

// observe sends only channel-level failures/successes to the breaker.
func (p *Prober) observe(m *store.Monitor, hasResp bool, code int, lat int64) {
	if p.breaker == nil {
		return
	}
	if !hasResp {
		p.breaker.ObserveProbe(m.UpstreamID, m.Model, false, 0)
		return
	}
	switch {
	case code >= 200 && code < 300:
		p.breaker.ObserveProbe(m.UpstreamID, m.Model, true, lat)
	case upstream.IsFailureStatus(code):
		p.breaker.ObserveProbe(m.UpstreamID, m.Model, false, lat)
	}
}

func validProbePayload(contentType string, payload []byte, stream bool) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}
	if stream || strings.HasPrefix(contentType, "text/event-stream") {
		text := string(trimmed)
		return strings.Contains(text, "data: [DONE]") ||
			strings.Contains(text, "event: message_stop") ||
			strings.Contains(text, "event: response.completed") ||
			strings.Contains(text, `"type":"message_stop"`) ||
			strings.Contains(text, `"type": "message_stop"`) ||
			strings.Contains(text, `"type":"response.completed"`) ||
			strings.Contains(text, `"type": "response.completed"`)
	}
	return json.Valid(trimmed) && !upstream.IsErrorPayload(trimmed)
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
