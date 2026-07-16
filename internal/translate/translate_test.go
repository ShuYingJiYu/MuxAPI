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
	if _, ok := body["messages"]; !ok {
		t.Fatalf("translated request has no messages: %s", exchange.UpstreamRequest)
	}
	if !exchange.UpstreamStream {
		t.Fatal("translated Claude non-stream response should be aggregated from upstream SSE")
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
