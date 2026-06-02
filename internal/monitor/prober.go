package monitor

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// Prober 监控探测器：周期遍历启用的监控项，按各自渠道+模型发最小请求，结果记入 Manager。
type Prober struct {
	mgr      *Manager
	store    *store.Store
	interval func() time.Duration // 动态探测间隔(页面可配)
	path     string
}

func NewProber(mgr *Manager, st *store.Store, interval func() time.Duration, path string) *Prober {
	if path == "" {
		path = "/v1/chat/completions"
	}
	return &Prober{mgr: mgr, store: st, interval: interval, path: path}
}

func (p *Prober) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.interval()): // 每轮取最新间隔，页面改了即时生效
			ms, _ := p.store.ListMonitors(true)
			for _, m := range ms {
				go p.Probe(ctx, m)
			}
		}
	}
}

// Probe 探测单个监控项一次。
func (p *Prober) Probe(ctx context.Context, m *store.Monitor) {
	body := []byte(`{"model":"` + m.Model + `","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(m.BaseURL, "/")+p.path, bytes.NewReader(body))
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
