package translate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestSourceFromPath(t *testing.T) {
	tests := map[string]Format{
		"/v1/chat/completions": OpenAI,
		"/v1/responses":        OpenAIResponses,
		"/v1/messages":         Claude,
	}
	for path, want := range tests {
		got, ok := SourceFromPath(path)
		if !ok || got != want {
			t.Fatalf("SourceFromPath(%q) = %q, %v; want %q, true", path, got, ok, want)
		}
	}
}

func TestSourceFromRequestDetectsNativeCodex(t *testing.T) {
	header := http.Header{"X-Codex-Turn-Metadata": []string{`{"turn_id":"turn_1"}`}}
	got, ok := SourceFromRequest("/v1/responses", header)
	if !ok || got != Codex {
		t.Fatalf("SourceFromRequest() = %q, %v; want %q, true", got, ok, Codex)
	}

	got, ok = SourceFromRequest("/v1/responses", http.Header{})
	if !ok || got != OpenAIResponses {
		t.Fatalf("generic Responses source = %q, %v; want %q, true", got, ok, OpenAIResponses)
	}

	got, ok = SourceFromRequest("/v1/messages", header)
	if !ok || got != Claude {
		t.Fatalf("Claude source = %q, %v; want %q, true", got, ok, Claude)
	}
}

func TestNewExchangeTranslatesResponsesRequestToClaude(t *testing.T) {
	original := []byte(`{"model":"gpt-test","input":"hello","stream":false}`)
	exchange, err := NewExchange(OpenAIResponses, Claude, "gpt-test", false, original)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(exchange.UpstreamRequest, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "gpt-test" {
		t.Fatalf("translated model = %v", body["model"])
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("translated request has no messages: %s", exchange.UpstreamRequest)
	}
	// 上游（如 New API）会校验 messages 非空，空数组等于请求缺字段。
	if len(messages) == 0 {
		t.Fatalf("translated request dropped the prompt: %s", exchange.UpstreamRequest)
	}
	if !exchange.UpstreamStream {
		t.Fatal("translated Claude non-stream response should be aggregated from upstream SSE")
	}
}

// Responses 的 input 允许字符串简写，SDK 翻译器只认数组，必须在边界归一化。
func TestNewExchangeNormalizesStringInputForEveryTarget(t *testing.T) {
	for _, target := range []Format{Claude, OpenAI, Codex} {
		exchange, err := NewExchange(OpenAIResponses, target, "gpt-test", true, []byte(`{"model":"gpt-test","input":"hello","stream":true}`))
		if err != nil {
			t.Fatalf("target %s: %v", target, err)
		}
		if !strings.Contains(string(exchange.UpstreamRequest), "hello") {
			t.Fatalf("target %s dropped the prompt: %s", target, exchange.UpstreamRequest)
		}
	}
}

func TestNewExchangeRejectsMissingPair(t *testing.T) {
	_, err := NewExchange(Claude, OpenAIResponses, "model", false, []byte(`{"model":"model","messages":[]}`))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v; want ErrUnsupported", err)
	}
}

func TestExchangeMaintainsStreamState(t *testing.T) {
	exchange, err := NewExchange(OpenAIResponses, Claude, "gpt-test", true, []byte(`{"model":"gpt-test","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	start, err := exchange.TranslateStream(context.Background(), []byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	joined := stringJoin(start)
	if !strings.Contains(joined, "response.created") {
		t.Fatalf("start events = %q", joined)
	}
	delta, err := exchange.TranslateStream(context.Background(), []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringJoin(delta), "hello") {
		t.Fatalf("delta events = %q", stringJoin(delta))
	}
}

func TestErrorResponseConvertsClaudeErrorToOpenAI(t *testing.T) {
	body := ErrorResponse(OpenAIResponses, http.StatusBadRequest, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad input"}}`))
	var response struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Message != "bad input" || response.Error.Type != "invalid_request_error" {
		t.Fatalf("response = %s", body)
	}
}

func TestErrorResponseConvertsOpenAIErrorToClaude(t *testing.T) {
	body := ErrorResponse(Claude, http.StatusNotFound, []byte(`{"error":{"message":"missing model","type":"not_found_error","code":"model_not_found"}}`))
	var response struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Type != "error" || response.Error.Message != "missing model" || response.Error.Type != "not_found_error" {
		t.Fatalf("response = %s", body)
	}
}

func stringJoin(chunks [][]byte) string {
	parts := make([]string, len(chunks))
	for i := range chunks {
		parts[i] = string(chunks[i])
	}
	return strings.Join(parts, "\n")
}
