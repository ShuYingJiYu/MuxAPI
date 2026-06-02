package health

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

// Prober 主动真实探测：定时对所有上游发最小 token 请求，结果反馈熔断器。
type Prober struct {
	mgr      *Manager
	list     func() []*upstream.Upstream
	interval func() time.Duration // 动态探测间隔(页面可配)
	model    string
	path     string // 探测端点，按上游协议配置(OpenAI:/v1/chat/completions, Claude:/v1/messages)
}

func NewProber(mgr *Manager, list func() []*upstream.Upstream, interval func() time.Duration, model, path string) *Prober {
	if path == "" {
		path = "/v1/chat/completions"
	}
	return &Prober{mgr: mgr, list: list, interval: interval, model: model, path: path}
}

// Run 阻塞循环，建议 go p.Run(ctx)。
func (p *Prober) Run(ctx context.Context) {
	for {
		iv := p.interval() // 每轮取最新间隔，页面改了下一轮生效
		select {
		case <-ctx.Done():
			return
		case <-time.After(iv):
			slog.Info("route probe round", "interval", iv.String())
			for _, u := range p.list() {
				go p.probe(ctx, u)
			}
		}
	}
}

// 最小 token 探测请求体
func (p *Prober) buildBody() []byte {
	if p.path == "/v1/messages" { // Claude 格式
		return []byte(`{"model":"` + p.model + `","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	}
	// OpenAI 格式（默认）
	return []byte(`{"model":"` + p.model + `","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
}

func (p *Prober) probe(ctx context.Context, u *upstream.Upstream) {
	p.mgr.markProbe(u.ID)
	req, err := u.BuildRequest(http.MethodPost, p.path, bytes.NewReader(p.buildBody()), http.Header{})
	if err != nil {
		p.mgr.reportProbe(u.ID, false, 0)
		return
	}
	req = req.WithContext(ctx)
	client := &http.Client{Timeout: 30 * time.Second, Transport: u.NewTransport()}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		p.mgr.reportProbe(u.ID, false, 0)
		return
	}
	defer resp.Body.Close()
	ok := resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests
	p.mgr.reportProbe(u.ID, ok, latency)
	slog.Debug("probe", "upstream", u.Name, "status", resp.StatusCode, "ok", ok, "ms", latency)
}
