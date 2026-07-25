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

// codexRequest 是 Codex CLI 真实请求体的最小形状：数组 input 加 codex 专有字段。
const codexRequest = `{"model":"m","input":[{"type":"message","role":"user",` +
	`"content":[{"type":"input_text","text":"hello"}]}],"stream":true,` +
	`"store":false,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"]}`

// SDK 没有注册 codex 作为来源，但 Codex CLI 的请求体本身就是合法 Responses 形状，
// 因此查表时降级成 openai-response，让 Codex 客户端也能落到 Claude/OpenAI 渠道。
func TestNewExchangeDegradesCodexSourceToResponses(t *testing.T) {
	for _, target := range []Format{Claude, OpenAI} {
		exchange, err := NewExchange(Codex, target, "m", true, []byte(codexRequest))
		if err != nil {
			t.Fatalf("target %s: %v", target, err)
		}
		if !strings.Contains(string(exchange.UpstreamRequest), "hello") {
			t.Fatalf("target %s dropped the prompt: %s", target, exchange.UpstreamRequest)
		}
		// codex 专有字段不应泄漏到上游请求里。
		// 注意 parallel_tool_calls 不在此列：它是 OpenAI Chat Completions 的合法参数，
		// 目标翻译器会有意映射它，透传过去反映的是客户端真实意图。
		for _, leaked := range []string{"reasoning.encrypted_content", `"store"`} {
			if strings.Contains(string(exchange.UpstreamRequest), leaked) {
				t.Fatalf("target %s leaked codex field %s: %s", target, leaked, exchange.UpstreamRequest)
			}
		}
		// Source 必须保持真实客户端协议，响应方向才能回到 Responses 形状。
		if exchange.Source != Codex {
			t.Fatalf("target %s mutated Source to %s", target, exchange.Source)
		}
	}
}

// Codex 客户端消费的是 Responses 形状的 SSE，所以响应方向必须把 Claude 事件
// 还原成 response.* 事件，否则请求通了客户端也读不懂。
func TestExchangeTranslatesClaudeStreamBackForCodexSource(t *testing.T) {
	exchange, err := NewExchange(Codex, Claude, "m", true, []byte(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	start, err := exchange.TranslateStream(context.Background(),
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringJoin(start), "response.created") {
		t.Fatalf("expected Responses-shaped start events, got %q", stringJoin(start))
	}
	delta, err := exchange.TranslateStream(context.Background(),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`))
	if err != nil {
		t.Fatal(err)
	}
	joined := stringJoin(delta)
	if !strings.Contains(joined, "OK") {
		t.Fatalf("delta lost the text: %q", joined)
	}
	if !strings.Contains(joined, "response.output_text.delta") {
		t.Fatalf("delta is not Responses-shaped: %q", joined)
	}
}

// 降级只用于查表，绝不能把 codex -> codex 从透传变成翻译。
func TestNewExchangeKeepsCodexToCodexPassthrough(t *testing.T) {
	exchange, err := NewExchange(Codex, Codex, "m", true, []byte(codexRequest))
	if err != nil {
		t.Fatal(err)
	}
	if exchange.Translated() {
		t.Fatal("codex -> codex must stay passthrough")
	}
	if string(exchange.UpstreamRequest) != codexRequest {
		t.Fatalf("codex -> codex altered the body: %s", exchange.UpstreamRequest)
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
