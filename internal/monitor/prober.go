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

// Prober 监控探测器：按各监控项自带的渠道+模型+探测参数发最小请求，结果记入 Manager。
// 每项可配自己的探测周期(interval_sec)、端点(path)、消息内容、max_tokens、是否流式；
// 这些字段为空/0 时回退到全局默认(interval/path)。
type Prober struct {
	mgr      *Manager
	store    *store.Store
	interval func() time.Duration // 全局默认探测间隔(页面可配)；监控项 interval_sec=0 时用它
	path     func() string        // 全局默认探测端点；监控项 path 为空时用它
}

func NewProber(mgr *Manager, st *store.Store, interval func() time.Duration, path func() string) *Prober {
	if path == nil {
		path = func() string { return "/v1/chat/completions" }
	}
	return &Prober{mgr: mgr, store: st, interval: interval, path: path}
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

// Probe 探测单个监控项一次。
func (p *Prober) Probe(ctx context.Context, m *store.Monitor) {
	path := m.Path
	if strings.TrimSpace(path) == "" {
		path = p.path()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(m.BaseURL, "/")+path, strings.NewReader(string(buildProbeBody(m))))
	if err != nil {
		p.mgr.Record(m.ID, statDown, 0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("x-api-key", m.APIKey)
	client := &http.Client{Timeout: 30 * time.Second, Transport: upstream.ProxyTransport(m.Proxy)}
	start := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		p.mgr.Record(m.ID, statDown, 0)
		return
	}
	defer resp.Body.Close()
	p.mgr.Record(m.ID, classify(resp.StatusCode), lat)
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
