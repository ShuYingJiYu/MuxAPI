package forward

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

// Health 转发层反馈结果给健康层
type Health interface {
	Report(id int64, ok bool, latencyMs int64)
}

// Logger 转发层记调用日志（status=HTTP码，网络失败为 0）
type Logger interface {
	Log(groupID, upstreamID int64, status int, latencyMs int64)
}

// Picker 调度层在指定分组内选上游
type Picker interface {
	Pick(groupID int64) (*upstream.Upstream, error)
}

type Forwarder struct {
	picker     Picker
	health     Health
	logger     Logger
	maxRetries int
}

func New(p Picker, h Health, l Logger, maxRetries int) *Forwarder {
	return &Forwarder{picker: p, health: h, logger: l, maxRetries: maxRetries}
}

// Forward 在指定分组内转发请求：失败则换上游重试（依赖调度层下次 Pick 自动避开已熔断的）。
func (f *Forwarder) Forward(w http.ResponseWriter, r *http.Request, body []byte, groupID int64) {
	var lastErr error
	tried := map[int64]bool{}
	for attempt := 0; attempt <= f.maxRetries; attempt++ {
		u, err := f.picker.Pick(groupID)
		if err != nil {
			http.Error(w, "no upstream available", http.StatusServiceUnavailable)
			return
		}
		if tried[u.ID] { // 同一上游不重复试，等熔断生效后下次 Pick 会换
			break
		}
		tried[u.ID] = true

		req, err := u.BuildRequest(r.Method, r.URL.Path, bytes.NewReader(body), r.Header)
		if err != nil {
			lastErr = err
			continue
		}
		req = req.WithContext(r.Context())
		// 每个上游用自己的代理出口；响应头超时防上游卡死(不影响已开始的流式)
		tr := u.NewTransport()
		tr.ResponseHeaderTimeout = 60 * time.Second
		client := &http.Client{Timeout: 0, Transport: tr}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			f.health.Report(u.ID, false, 0)
			f.logger.Log(groupID, u.ID, 0, 0)
			lastErr = err
			continue
		}
		// 上游 5xx/429 视为失败，反馈并重试下一个
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lat := time.Since(start).Milliseconds()
			f.health.Report(u.ID, false, lat)
			f.logger.Log(groupID, u.ID, resp.StatusCode, lat)
			resp.Body.Close()
			continue
		}
		// 成功：反馈 + 透传响应
		lat := time.Since(start).Milliseconds()
		f.health.Report(u.ID, true, lat)
		f.logger.Log(groupID, u.ID, resp.StatusCode, lat)
		relayResponse(w, resp)
		return
	}
	if lastErr != nil {
		http.Error(w, "upstream error: "+lastErr.Error(), http.StatusBadGateway)
		return
	}
	http.Error(w, "all upstreams failed", http.StatusBadGateway)
}

// relayResponse 原样透传上游响应，流式则逐行 flush（照抄 sub2api 思路）。
func relayResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	ct := resp.Header.Get("Content-Type")
	w.WriteHeader(resp.StatusCode)

	// 用首字节判断真实流式，避免上游 Content-Type 不可靠导致误判。
	flusher, canFlush := w.(http.Flusher)
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	if canFlush && isStream(ct, br) {
		sc := bufio.NewScanner(br)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			w.Write(sc.Bytes())
			w.Write([]byte("\n"))
			flusher.Flush()
		}
		return
	}
	io.Copy(w, br)
}

// isStream 真实流式判定：Content-Type 标了 SSE 且响应体确以 data:/event: 开头。
func isStream(ct string, br *bufio.Reader) bool {
	if !strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	head, _ := br.Peek(6)
	return bytes.HasPrefix(head, []byte("data:")) || bytes.HasPrefix(head, []byte("event:"))
}
