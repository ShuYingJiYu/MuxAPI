package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResponseAuditParsesSplitResponsesEvent(t *testing.T) {
	audit := &responseAudit{stream: true}
	parts := []string{
		"event: response.comp",
		"leted\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{",
		"\"input_tokens\":12,\"output_tokens\":7,\"cache_creation_input_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":3}}}}\r\n\r\n",
	}
	for _, part := range parts {
		audit.feed([]byte(part))
	}
	audit.finish()
	if !audit.streamCompleted || audit.lastEvent != "response.completed" {
		t.Fatalf("completion audit mismatch: completed=%v event=%q", audit.streamCompleted, audit.lastEvent)
	}
	if audit.usage.input != 12 || audit.usage.output != 7 || audit.usage.cached != 3 || audit.usage.cacheCreation != 5 {
		t.Fatalf("usage audit mismatch: %+v", audit.usage)
	}
}

func TestUsageAuditParsesAnthropicCacheTokens(t *testing.T) {
	usage := usageFromJSON([]byte(`{"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":30,"cache_creation_input_tokens":6}}`))
	if usage.input != 10 || usage.output != 4 || usage.cached != 30 || usage.cacheCreation != 6 {
		t.Fatalf("anthropic usage mismatch: %+v", usage)
	}
}

func TestContentBlockStopIsNotWholeStreamCompletion(t *testing.T) {
	audit := &responseAudit{stream: true}
	audit.feed([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\"}\n\n"))
	if audit.streamCompleted {
		t.Fatal("content_block_stop must not mark the whole message complete")
	}
	audit.feed([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	if !audit.streamCompleted || audit.lastEvent != "message_stop" {
		t.Fatalf("message_stop should complete the stream: %+v", audit)
	}
}

func TestRelayResponseCapturesUsageBytesAndRequestID(t *testing.T) {
	body := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2}}}`
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("X-Request-ID", "upstream-123")
	resp := &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}
	recorder := httptest.NewRecorder()
	result := relayResponse(recorder, resp, time.Now(), nil)
	if result.err != nil || result.bytesSent != int64(len(body)) {
		t.Fatalf("relay result mismatch: %+v", result)
	}
	if result.usage.input != 9 || result.usage.output != 4 || result.usage.cached != 2 {
		t.Fatalf("usage mismatch: %+v", result.usage)
	}
	if result.upstreamRequestID != "upstream-123" {
		t.Fatalf("request id mismatch: %q", result.upstreamRequestID)
	}
}
