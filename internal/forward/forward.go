package forward

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

// Health 转发层反馈结果给健康层（model 为空时按上游级处理）
type Health interface {
	Report(id int64, model string, ok bool, latencyMs int64)
}

// Logger 转发层记调用日志（status=HTTP码，网络失败为 0；model 为请求模型，解析失败为空）
type Logger interface {
	Log(groupID, upstreamID int64, model, endpoint, keyName string, retries, status int, latencyMs int64)
}

// Picker 调度层在指定分组内按模型选上游（exclude 为本次请求已试过、需跳过的上游）
type Picker interface {
	PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, error)
}

type Forwarder struct {
	picker     Picker
	health     Health
	logger     Logger
	maxRetries int
	// 首响应头超时(容忍线)：超过即视为该上游失败、cancel 换源。
	// 用首响应头而非总时长——流式长输出总时长几十秒正常，砍总时长会误杀；
	// 首字节(TTFT)能切掉「上游号池轮询卡住」这类慢。nil 时回退默认 60s。
	tolerance func() time.Duration
}

func New(p Picker, h Health, l Logger, maxRetries int) *Forwarder {
	return &Forwarder{picker: p, health: h, logger: l, maxRetries: maxRetries}
}

// SetTolerance 注入首响应头超时(容忍线)，读 settings 即时生效。
func (f *Forwarder) SetTolerance(d func() time.Duration) { f.tolerance = d }

// headerTimeout 当前容忍线；未注入或非正值时回退默认 60s。
func (f *Forwarder) headerTimeout() time.Duration {
	if f.tolerance != nil {
		if d := f.tolerance(); d > 0 {
			return d
		}
	}
	return 60 * time.Second
}

// Forward 在指定分组内转发请求：失败则换上游重试（每次跳过本次已试过的，立即切换到下一优先级层）。
// keyName 为命中的接入密钥名，仅用于请求记录展示来源客户端。
func (f *Forwarder) Forward(w http.ResponseWriter, r *http.Request, body []byte, groupID int64, keyName string) {
	model := parseModel(body) // 解析请求模型用于 (上游,模型) 级健康判定；解析失败为 "" 回退上游级
	endpoint := r.URL.Path    // 请求端点(如 /v1/messages)，落库供按协议区分
	var lastErr error
	tried := map[int64]bool{}
	for attempt := 0; attempt <= f.maxRetries; attempt++ {
		u, err := f.picker.PickExcluding(groupID, model, tried)
		if err != nil {
			break // 没有更多可用上游了
		}
		tried[u.ID] = true

		req, err := u.BuildRequest(r.Method, r.URL.Path, bytes.NewReader(body), r.Header)
		if err != nil {
			lastErr = err
			continue
		}
		// 子 context 仅用于「首响应头超时」(容忍线)：超时即 cancel 换源。
		// 不在共享 Transport 上设 ResponseHeaderTimeout——那会按代理出口被所有请求共享、
		// 且容忍线动态可变。改用定时器：拿到响应头后立刻 Stop，故不影响已开始的流式总时长。
		// 父 ctx(r.Context()) 反映客户端是否断连，与本超时各自独立、便于区分二者。
		ctx, cancel := context.WithCancel(r.Context())
		timer := time.AfterFunc(f.headerTimeout(), cancel)
		req = req.WithContext(ctx)
		// 每个上游用自己的代理出口共享 Transport（含空闲回收，避免每请求新建泄漏连接）。
		tr := u.NewTransport()
		client := &http.Client{Timeout: 0, Transport: tr}
		start := time.Now()
		resp, err := client.Do(req)
		timer.Stop() // 已收到响应头(或已失败)，解除超时定时器，后续流式不受限
		if err != nil {
			// 客户端断连(父 ctx 已取消)：直接结束，不上报健康、不记日志、不重试——
			// 否则一次 abort 会让本次试过的多个健康上游各记一次失败、累积误熔断。
			if r.Context().Err() != nil {
				cancel()
				return
			}
			// 首响应头超时(本地 cancel 触发、父 ctx 未取消) 或真实网络错误：
			// 均视为该上游模型级失败，反馈并换下一个上游。
			cancel()
			f.health.Report(u.ID, model, false, 0)
			f.logger.Log(groupID, u.ID, model, endpoint, keyName, attempt, 0, 0)
			lastErr = err
			continue
		}
		// 上游级失败(5xx/429/401/402/403/408)：反馈并切换下一个上游
		if upstream.IsFailureStatus(resp.StatusCode) {
			lat := time.Since(start).Milliseconds()
			f.health.Report(u.ID, failScope(model, resp.StatusCode), false, lat)
			f.logger.Log(groupID, u.ID, model, endpoint, keyName, attempt, resp.StatusCode, lat)
			io.Copy(io.Discard, resp.Body) // 排空再 Close，助连接复用
			resp.Body.Close()
			cancel()
			continue
		}
		// 成功：反馈 + 透传响应
		lat := time.Since(start).Milliseconds()
		f.health.Report(u.ID, model, true, lat)
		f.logger.Log(groupID, u.ID, model, endpoint, keyName, attempt, resp.StatusCode, lat)
		relayResponse(w, resp)
		cancel()
		return
	}
	// 循环结束仍没成功：分两种情况
	if len(tried) == 0 {
		http.Error(w, "no upstream available", http.StatusServiceUnavailable)
		return
	}
	if lastErr != nil {
		http.Error(w, "upstream error: "+lastErr.Error(), http.StatusBadGateway)
		return
	}
	http.Error(w, "all upstreams failed", http.StatusBadGateway)
}

// parseModel 从请求体浅解析 model 字段，用于 (上游,模型) 级健康判定。
// 解析失败/缺失返回 ""，调用方据此回退到上游级（行为等价于改造前）。
func parseModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// failScope 按失败原因决定熔断范围：凭证/余额类(401/402/403)熔断整上游(返回"")，
// 其余(5xx/429/408)仅熔断当前模型(返回 model)。见 upstream.FailIsUpstreamLevel。
func failScope(model string, code int) string {
	if upstream.FailIsUpstreamLevel(code) {
		return ""
	}
	return model
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
		// 按行读 + 实时 flush：ReadBytes 不受 Scanner 的 token 上限约束，
		// 单条 data: 行再长也完整透传，不会静默截断。读到 EOF 即结束。
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				w.Write(line)
				flusher.Flush()
			}
			if err != nil { // io.EOF 或读错误：流结束
				return
			}
		}
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
