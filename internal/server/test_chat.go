package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// testEvent 推给前端的测试事件（仿 sub2api TestEvent）。
type testEvent struct {
	Type      string `json:"type"` // test_start | content | test_complete | error
	Model     string `json:"model,omitempty"`
	Text      string `json:"text,omitempty"`
	Success   bool   `json:"success,omitempty"`
	Status    int    `json:"status,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// testUpstreamChat 发一条真实 chat 请求走完整转发链路，SSE 逐块回显上游真实回复。
// 这是端到端验证（能否真对话），区别于 /models 的「凭证能列模型」轻量探测。
func (s *Server) testUpstreamChat(w http.ResponseWriter, r *http.Request, id int64) {
	u, err := s.store.Get(id)
	if err != nil {
		http.Error(w, "upstream not found", 404)
		return
	}
	model := r.URL.Query().Get("model")
	if model == "" {
		model = "gpt-5.5"
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	send := func(e testEvent) {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	send(testEvent{Type: "test_start", Model: model})

	payload := fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":64,"stream":true}`,
		model)
	req, err := u.BuildRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload), http.Header{})
	if err != nil {
		send(testEvent{Type: "error", Error: err.Error()})
		return
	}
	req = req.WithContext(r.Context())

	start := time.Now()
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		send(testEvent{Type: "error", Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	defer resp.Body.Close()

	// 非 2xx：透传上游真实状态码与错误体（402 余额 / 403 封禁 / 429 限流等）。
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		send(testEvent{
			Type:      "error",
			Status:    resp.StatusCode,
			Error:     strings.TrimSpace(string(body)),
			LatencyMs: time.Since(start).Milliseconds(),
		})
		return
	}

	s.relayChatStream(resp.Body, send, start)
}

// relayChatStream 解析上游 OpenAI SSE 流，逐块把 delta 文字转成 content 事件。
func (s *Server) relayChatStream(body io.Reader, send func(testEvent), start time.Time) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			send(testEvent{Type: "error", Error: chunk.Error.Message, LatencyMs: time.Since(start).Milliseconds()})
			return
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				send(testEvent{Type: "content", Text: ch.Delta.Content})
			}
		}
	}
	send(testEvent{Type: "test_complete", Success: true, LatencyMs: time.Since(start).Milliseconds()})
}
