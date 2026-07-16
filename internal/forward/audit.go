package forward

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxAuditPayload = 2 << 20

type tokenUsage struct {
	input  int64
	output int64
	cached int64
}

func (u *tokenUsage) merge(other tokenUsage) {
	if other.input > u.input {
		u.input = other.input
	}
	if other.output > u.output {
		u.output = other.output
	}
	if other.cached > u.cached {
		u.cached = other.cached
	}
}

type responseAudit struct {
	stream          bool
	body            []byte
	overflow        bool
	sseBuffer       []byte
	usage           tokenUsage
	lastEvent       string
	streamCompleted bool
}

type auditReadCloser struct {
	io.ReadCloser
	audit *responseAudit
}

func newAuditReadCloser(body io.ReadCloser, stream bool) *auditReadCloser {
	return &auditReadCloser{ReadCloser: body, audit: &responseAudit{stream: stream}}
}

func (r *auditReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.audit.feed(p[:n])
	}
	if err == io.EOF {
		r.audit.finish()
	}
	return n, err
}

func (a *responseAudit) feed(chunk []byte) {
	if a.stream {
		a.sseBuffer = append(a.sseBuffer, chunk...)
		for {
			end, skip := nextSSEBlock(a.sseBuffer)
			if end < 0 {
				break
			}
			a.consumeSSEBlock(a.sseBuffer[:end])
			a.sseBuffer = a.sseBuffer[end+skip:]
		}
		if len(a.sseBuffer) > maxAuditPayload {
			a.sseBuffer = append([]byte(nil), a.sseBuffer[len(a.sseBuffer)-maxAuditPayload:]...)
		}
		return
	}
	if a.overflow {
		return
	}
	if len(a.body)+len(chunk) > maxAuditPayload {
		a.body = nil
		a.overflow = true
		return
	}
	a.body = append(a.body, chunk...)
}

func (a *responseAudit) finish() {
	if a.stream {
		if len(bytes.TrimSpace(a.sseBuffer)) > 0 {
			a.consumeSSEBlock(a.sseBuffer)
		}
		a.sseBuffer = nil
		return
	}
	if !a.overflow {
		a.usage.merge(usageFromJSON(a.body))
	}
}

func nextSSEBlock(data []byte) (end, separatorLength int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case lf >= 0 && (crlf < 0 || lf < crlf):
		return lf, 2
	default:
		return crlf, 4
	}
}

func (a *responseAudit) consumeSSEBlock(block []byte) {
	var eventName string
	var dataLines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if eventName != "" {
		a.lastEvent = clipLabel(eventName)
		if isCompletionEvent(eventName) {
			a.streamCompleted = true
		}
	}
	data := strings.Join(dataLines, "\n")
	if data == "" {
		return
	}
	if data == "[DONE]" {
		a.lastEvent = "[DONE]"
		a.streamCompleted = true
		return
	}
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return
	}
	if typ, _ := payload["type"].(string); typ != "" {
		a.lastEvent = clipLabel(typ)
		if isCompletionEvent(typ) {
			a.streamCompleted = true
		}
	}
	a.usage.merge(usageFromValue(payload))
}

func isCompletionEvent(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "response.completed", "message_stop", "message.completed", "done", "[done]":
		return true
	default:
		return false
	}
}

func clipLabel(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 80 {
		return value[:80]
	}
	return value
}

func usageFromJSON(data []byte) tokenUsage {
	var value any
	if len(bytes.TrimSpace(data)) == 0 || json.Unmarshal(data, &value) != nil {
		return tokenUsage{}
	}
	return usageFromValue(value)
}

func usageFromValue(value any) tokenUsage {
	root, ok := value.(map[string]any)
	if !ok {
		return tokenUsage{}
	}
	var out tokenUsage
	mergeUsageObject := func(candidate any) {
		if object, ok := candidate.(map[string]any); ok {
			out.merge(parseUsageObject(object))
		}
	}
	mergeUsageObject(root["usage"])
	for _, key := range []string{"response", "message"} {
		if nested, ok := root[key].(map[string]any); ok {
			mergeUsageObject(nested["usage"])
		}
	}
	return out
}

func parseUsageObject(usage map[string]any) tokenUsage {
	input := maxInt64(number(usage["input_tokens"]), number(usage["prompt_tokens"]))
	output := maxInt64(number(usage["output_tokens"]), number(usage["completion_tokens"]))
	cached := maxInt64(number(usage["cached_tokens"]), number(usage["cache_read_input_tokens"]))
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if details, ok := usage[key].(map[string]any); ok {
			cached = maxInt64(cached, number(details["cached_tokens"]))
		}
	}
	return tokenUsage{input: input, output: output, cached: cached}
}

func number(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case json.Number:
		v, _ := n.Int64()
		return v
	default:
		return 0
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func upstreamRequestID(header http.Header) string {
	for _, key := range []string{"X-Request-ID", "Request-ID", "OpenAI-Request-ID", "CF-Ray"} {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return clipLabel(value)
		}
	}
	return ""
}
