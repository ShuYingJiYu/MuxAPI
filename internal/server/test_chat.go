package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/translate"
)

// testEvent 是管理后台“测试上游”接口向浏览器发送的统一事件格式。
type testEvent struct {
	Type      string `json:"type"` // test_start | content | test_complete | error
	Model     string `json:"model,omitempty"`
	Text      string `json:"text,omitempty"`
	Success   bool   `json:"success,omitempty"`
	Status    int    `json:"status,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// testUpstreamChat sends a canonical Responses request through the configured
// protocol translator, then converts the result back before reporting it.
func (s *Server) testUpstreamChat(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u, err := s.store.Get(id)
	if err != nil {
		http.Error(w, "upstream not found", http.StatusNotFound)
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		model = "gpt-5.5"
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(e testEvent) {
		payload, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}
	send(testEvent{Type: "test_start", Model: model})

	target, valid := translate.NormalizeFormat(u.Protocol)
	if !valid {
		send(testEvent{Type: "error", Error: "unsupported upstream protocol: " + u.Protocol})
		return
	}
	original, _ := json.Marshal(map[string]any{
		"model":             model,
		"input":             "Reply with OK.",
		"max_output_tokens": 64,
		"stream":            true,
	})
	exchange, err := translate.NewExchange(translate.OpenAIResponses, target, model, true, original)
	if err != nil {
		send(testEvent{Type: "error", Error: err.Error()})
		return
	}
	path, err := translate.TargetPath(target, "/v1/responses")
	if err != nil {
		send(testEvent{Type: "error", Error: err.Error()})
		return
	}
	req, err := u.BuildRequest(http.MethodPost, path, bytes.NewReader(exchange.UpstreamRequest), http.Header{
		"Accept": []string{"text/event-stream"},
	})
	if err != nil {
		send(testEvent{Type: "error", Error: err.Error()})
		return
	}
	translate.ConfigureRequestHeaders(req.Header, target, exchange.Translated())
	req = req.WithContext(r.Context())

	start := time.Now()
	resp, err := (&http.Client{Timeout: 60 * time.Second, Transport: u.NewTransport()}).Do(req)
	if err != nil {
		send(testEvent{Type: "error", Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		send(testEvent{
			Type:      "error",
			Status:    resp.StatusCode,
			Error:     strings.TrimSpace(string(body)),
			LatencyMs: time.Since(start).Milliseconds(),
		})
		return
	}

	// 无论上游返回流还是完整 JSON，最终都转换成 testEvent 流供界面消费。
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		s.relayTranslatedTestStream(r.Context(), resp.Body, exchange, send, start)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		send(testEvent{Type: "error", Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	if exchange.Translated() {
		body, err = exchange.TranslateNonStream(r.Context(), body)
		if err != nil {
			send(testEvent{Type: "error", Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
			return
		}
	}
	s.relayTestResponseBody(body, send, start)
}

// relayTranslatedTestStream 逐行转换上游 SSE，并在终态事件后立即结束测试。
func (s *Server) relayTranslatedTestStream(ctx context.Context, body io.Reader, exchange *translate.Exchange, send func(testEvent), start time.Time) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 52_428_800)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		upstreamDone := bytes.Equal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), []byte("[DONE]"))
		chunks, err := exchange.TranslateStream(ctx, line)
		if err != nil {
			send(testEvent{Type: "error", Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
			return
		}
		for _, chunk := range chunks {
			terminal, failed := emitTestResponseEvents(chunk, send, start)
			if terminal || failed {
				return
			}
		}
		if upstreamDone {
			send(testEvent{Type: "test_complete", Success: true, LatencyMs: time.Since(start).Milliseconds()})
			return
		}
	}
	if err := scanner.Err(); err != nil {
		send(testEvent{Type: "error", Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	send(testEvent{Type: "error", Error: "stream ended before response.completed", LatencyMs: time.Since(start).Milliseconds()})
}

// relayTestResponseBody 从标准 Responses JSON 中提取文本和错误。
func (s *Server) relayTestResponseBody(body []byte, send func(testEvent), start time.Time) {
	var response struct {
		Error  json.RawMessage `json:"error"`
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		send(testEvent{Type: "error", Error: "invalid translated response: " + err.Error(), LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	if message := responseErrorMessage(response.Error); message != "" {
		send(testEvent{Type: "error", Error: message, LatencyMs: time.Since(start).Milliseconds()})
		return
	}
	for _, item := range response.Output {
		for _, content := range item.Content {
			if content.Text != "" {
				send(testEvent{Type: "content", Text: content.Text})
			}
		}
	}
	send(testEvent{Type: "test_complete", Success: true, LatencyMs: time.Since(start).Milliseconds()})
}

// emitTestResponseEvents 将标准 Responses SSE 事件压缩成界面需要的三类事件。
func emitTestResponseEvents(chunk []byte, send func(testEvent), start time.Time) (terminal, failed bool) {
	lines := strings.Split(strings.ReplaceAll(string(chunk), "\r\n", "\n"), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			send(testEvent{Type: "test_complete", Success: true, LatencyMs: time.Since(start).Milliseconds()})
			return true, false
		}
		if !json.Valid([]byte(data)) {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Message  string          `json:"message"`
			Error    json.RawMessage `json:"error"`
			Response struct {
				Error json.RawMessage `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event.Type {
		case "response.output_text.delta":
			if event.Delta != "" {
				send(testEvent{Type: "content", Text: event.Delta})
			}
		case "response.completed":
			send(testEvent{Type: "test_complete", Success: true, LatencyMs: time.Since(start).Milliseconds()})
			return true, false
		case "response.failed", "response.incomplete", "error":
			message := responseErrorMessage(event.Error)
			if message == "" {
				message = responseErrorMessage(event.Response.Error)
			}
			if message == "" {
				message = event.Message
			}
			if message == "" {
				message = event.Type
			}
			send(testEvent{Type: "error", Error: message, LatencyMs: time.Since(start).Milliseconds()})
			return false, true
		}
	}
	return false, false
}

// responseErrorMessage 兼容字符串错误和常见的结构化错误对象。
func responseErrorMessage(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return message
	}
	var object struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(raw, &object) == nil {
		if object.Message != "" {
			return object.Message
		}
		if object.Code != "" {
			return object.Code
		}
		if object.Type != "" {
			return object.Type
		}
	}
	return string(raw)
}
