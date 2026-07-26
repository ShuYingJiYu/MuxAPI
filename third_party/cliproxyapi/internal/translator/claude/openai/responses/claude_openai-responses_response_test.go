package responses

import (
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func parseClaudeResponsesSSEEvent(t *testing.T, chunk []byte) (string, gjson.Result) {
	t.Helper()

	var event string
	var data string
	for _, line := range strings.Split(string(chunk), "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if data == "" {
		t.Fatalf("SSE chunk has no data line: %s", string(chunk))
	}

	return event, gjson.Parse(data)
}

func translateClaudeResponsesStreamThroughRegistry(chunks [][]byte) [][]byte {
	var param any
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, "claude-test", nil, nil, chunk, &param)...)
	}
	return outputs
}

func TestConvertClaudeResponseToOpenAIResponses_ThinkingIncludesSignature(t *testing.T) {
	signature := "claude_sig_123"
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"internal "}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param)...)
	}

	var reasoningDone gjson.Result
	var completed gjson.Result
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		switch event {
		case "response.output_item.done":
			if data.Get("item.type").String() == "reasoning" {
				reasoningDone = data
			}
		case "response.completed":
			completed = data
		}
	}

	if !reasoningDone.Exists() {
		t.Fatal("expected reasoning output_item.done event")
	}
	if got := reasoningDone.Get("item.encrypted_content").String(); got != signature {
		t.Fatalf("reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := reasoningDone.Get("item.summary.0.text").String(); got != "internal reasoning" {
		t.Fatalf("reasoning summary text = %q", got)
	}
	if got := completed.Get("response.output.0.encrypted_content").String(); got != signature {
		t.Fatalf("completed reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := completed.Get("response.output.0.summary.0.text").String(); got != "internal reasoning" {
		t.Fatalf("completed reasoning summary text = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_SuppressesSignatureDeltaPassthrough(t *testing.T) {
	chunk := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"claude_sig_123"}}`)

	outputs := translateClaudeResponsesStreamThroughRegistry([][]byte{chunk})
	if len(outputs) != 0 {
		t.Fatalf("expected signature_delta to be suppressed, got %d chunks", len(outputs))
	}
}

func TestConvertClaudeResponseToOpenAIResponses_AggregatesTextBlocksUntilMessageStop(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":4,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"**Compare competitors**\n- "}}`),
		[]byte(`data: {"type":"content_block_stop","index":4}`),
		[]byte(`data: {"type":"content_block_start","index":5,"content_block":{"type":"server_tool_use","id":"srv_123","name":"web_search","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"Qwen3\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":5}`),
		[]byte(`data: {"type":"content_block_start","index":6,"content_block":{"type":"web_search_tool_result","tool_use_id":"srv_123","content":[{"type":"web_search_result","title":"Example","url":"https://example.com"}]}}`),
		[]byte(`data: {"type":"content_block_stop","index":6}`),
		[]byte(`data: {"type":"content_block_delta","index":5,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","cited_text":"Qwen 3.7 Max","url":"https://example.com","title":"Example"}}}`),
		[]byte(`data: {"type":"content_block_start","index":7,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"Qwen 3.7 Max leads."}}`),
		[]byte(`data: {"type":"content_block_stop","index":7}`),
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":12}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	outputs := translateClaudeResponsesStreamThroughRegistry(chunks)

	counts := map[string]int{}
	var outputTextDone gjson.Result
	var completed gjson.Result
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		counts[event]++
		if event == "response.output_text.done" {
			outputTextDone = data
		}
		if event == "response.completed" {
			completed = data
		}
		if strings.HasPrefix(event, "content_block_") || event == "message_delta" {
			t.Fatalf("unexpected anthropic-native event leaked: %s", event)
		}
	}

	if counts["response.output_item.added"] != 1 {
		t.Fatalf("response.output_item.added count = %d, want 1", counts["response.output_item.added"])
	}
	if counts["response.content_part.added"] != 1 {
		t.Fatalf("response.content_part.added count = %d, want 1", counts["response.content_part.added"])
	}
	if counts["response.output_text.done"] != 1 {
		t.Fatalf("response.output_text.done count = %d, want 1", counts["response.output_text.done"])
	}
	if counts["response.content_part.done"] != 1 {
		t.Fatalf("response.content_part.done count = %d, want 1", counts["response.content_part.done"])
	}
	if counts["response.output_item.done"] != 1 {
		t.Fatalf("response.output_item.done count = %d, want 1", counts["response.output_item.done"])
	}
	if counts["response.function_call_arguments.delta"] != 0 {
		t.Fatalf("response.function_call_arguments.delta count = %d, want 0", counts["response.function_call_arguments.delta"])
	}

	wantText := "**Compare competitors**\n- Qwen 3.7 Max leads."
	if got := outputTextDone.Get("text").String(); got != wantText {
		t.Fatalf("output_text.done text = %q, want %q", got, wantText)
	}
	if got := completed.Get("response.output.0.content.0.text").String(); got != wantText {
		t.Fatalf("completed message text = %q, want %q", got, wantText)
	}
	if got := completed.Get("response.output.0.content.0.annotations.0.type").String(); got != "web_search_result_location" {
		t.Fatalf("completed annotation type = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_ReportsCacheTokens(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":13,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}}`),
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":4,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.completed" {
				completed = data
			}
		}
	}

	if !completed.Exists() {
		t.Fatal("expected response.completed event")
	}
	if got := completed.Get("response.usage.input_tokens").Int(); got != 22044 {
		t.Fatalf("response usage input_tokens = %d, want %d", got, 22044)
	}
	if got := completed.Get("response.usage.input_tokens_details.cached_tokens").Int(); got != 22000 {
		t.Fatalf("response usage cached_tokens = %d, want %d", got, 22000)
	}
	if got := completed.Get("response.usage.output_tokens").Int(); got != 4 {
		t.Fatalf("response usage output_tokens = %d, want %d", got, 4)
	}
	if got := completed.Get("response.usage.total_tokens").Int(); got != 22048 {
		t.Fatalf("response usage total_tokens = %d, want %d", got, 22048)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_ThinkingIncludesSignature(t *testing.T) {
	signature := "claude_sig_nonstream"
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"nonstream reasoning"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.0.encrypted_content").String(); got != signature {
		t.Fatalf("non-stream reasoning encrypted_content = %q, want %q", got, signature)
	}
	if got := root.Get("output.0.summary.0.text").String(); got != "nonstream reasoning" {
		t.Fatalf("non-stream reasoning summary text = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_ReportsCacheTokens(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_nonstream","usage":{"input_tokens":13,"output_tokens":1,"cache_read_input_tokens":22000,"cache_creation_input_tokens":31}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("usage.input_tokens").Int(); got != 22044 {
		t.Fatalf("non-stream usage input_tokens = %d, want %d", got, 22044)
	}
	if got := root.Get("usage.input_tokens_details.cached_tokens").Int(); got != 22000 {
		t.Fatalf("non-stream usage cached_tokens = %d, want %d", got, 22000)
	}
	if got := root.Get("usage.output_tokens").Int(); got != 4 {
		t.Fatalf("non-stream usage output_tokens = %d, want %d", got, 4)
	}
	if got := root.Get("usage.total_tokens").Int(); got != 22048 {
		t.Fatalf("non-stream usage total_tokens = %d, want %d", got, 22048)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_RestoresNamespaceFunctionCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__node_repl",
				"tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}]
			}
		]
	}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_abc","name":"mcp__node_repl__js","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{"code":"nodeRepl.write('hello')"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var added gjson.Result
	var done gjson.Result
	var completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", originalRequest, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			switch event {
			case "response.output_item.added":
				if data.Get("item.type").String() == "function_call" {
					added = data
				}
			case "response.output_item.done":
				if data.Get("item.type").String() == "function_call" {
					done = data
				}
			case "response.completed":
				completed = data
			}
		}
	}

	for _, tc := range []struct {
		label string
		got   gjson.Result
	}{
		{"added", added},
		{"done", done},
	} {
		if !tc.got.Exists() {
			t.Fatalf("expected function_call %s event", tc.label)
		}
		if got := tc.got.Get("item.name").String(); got != "js" {
			t.Fatalf("%s item.name = %q, want js", tc.label, got)
		}
		if got := tc.got.Get("item.namespace").String(); got != "mcp__node_repl" {
			t.Fatalf("%s item.namespace = %q, want mcp__node_repl", tc.label, got)
		}
	}

	if !completed.Exists() {
		t.Fatal("expected response.completed event")
	}
	if got := completed.Get("response.output.0.name").String(); got != "js" {
		t.Fatalf("completed output name = %q, want js", got)
	}
	if got := completed.Get("response.output.0.namespace").String(); got != "mcp__node_repl" {
		t.Fatalf("completed output namespace = %q, want mcp__node_repl", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_RestoresNamespaceFunctionCall(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-test",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__node_repl",
				"tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}]
			}
		]
	}`)
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_nonstream","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_abc","name":"mcp__node_repl__js","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"code\":\"nodeRepl.write('hello')\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", originalRequest, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.0.name").String(); got != "js" {
		t.Fatalf("non-stream output name = %q, want js", got)
	}
	if got := root.Get("output.0.namespace").String(); got != "mcp__node_repl" {
		t.Fatalf("non-stream output namespace = %q, want mcp__node_repl", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_ErrorEventEmitsResponseFailed(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_err","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		[]byte(`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	}

	var param any
	var failed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.failed" {
				failed = data
			}
		}
	}

	if !failed.Exists() {
		t.Fatal("expected response.failed event after upstream error")
	}
	if got := failed.Get("response.status").String(); got != "failed" {
		t.Fatalf("response.status = %q, want failed", got)
	}
	if got := failed.Get("response.error.message").String(); got != "Overloaded" {
		t.Fatalf("response.error.message = %q, want Overloaded", got)
	}
	if got := failed.Get("response.error.code").String(); got != "overloaded_error" {
		t.Fatalf("response.error.code = %q, want overloaded_error", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_ErrorBeforeMessageStartEmitsNothing(t *testing.T) {
	var param any
	outputs := ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil,
		[]byte(`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), &param)
	if len(outputs) != 0 {
		t.Fatalf("error before message_start should emit nothing so the gateway can fail over, got %d chunks", len(outputs))
	}
}

func TestConvertClaudeResponseToOpenAIResponses_NoSecondTerminalAfterError(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_err2","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		[]byte(`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var events []string
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param) {
			event, _ := parseClaudeResponsesSSEEvent(t, output)
			events = append(events, event)
		}
	}

	sawFailed := false
	for _, event := range events {
		switch event {
		case "response.failed":
			sawFailed = true
		case "response.completed", "response.incomplete":
			t.Fatalf("no second terminal event may follow response.failed, got %v", events)
		}
	}
	if !sawFailed {
		t.Fatalf("expected response.failed, got %v", events)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_MaxTokensEmitsResponseIncomplete(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_trunc","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut off"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":9}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var incomplete gjson.Result
	sawCompleted := false
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.incomplete" {
				incomplete = data
			}
			if event == "response.completed" {
				sawCompleted = true
			}
		}
	}

	if sawCompleted {
		t.Fatal("truncated response must not be reported as response.completed")
	}
	if !incomplete.Exists() {
		t.Fatal("expected response.incomplete event for stop_reason max_tokens")
	}
	if got := incomplete.Get("response.status").String(); got != "incomplete" {
		t.Fatalf("response.status = %q, want incomplete", got)
	}
	if got := incomplete.Get("response.incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason = %q, want max_output_tokens", got)
	}
	if got := incomplete.Get("response.output.0.content.0.text").String(); got != "cut off" {
		t.Fatalf("incomplete response should still carry aggregated output, got %q", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_MultipleThinkingBlocksAllInFinalOutput(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_multi","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_one"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"second"}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig_two"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.completed" {
				completed = data
			}
		}
	}

	if got := completed.Get("response.output.#").Int(); got != 2 {
		t.Fatalf("completed output count = %d, want 2 reasoning items. Output: %s", got, completed.Raw)
	}
	if got := completed.Get("response.output.0.encrypted_content").String(); got != "sig_one" {
		t.Fatalf("output.0 encrypted_content = %q, want sig_one", got)
	}
	if got := completed.Get("response.output.0.summary.0.text").String(); got != "first" {
		t.Fatalf("output.0 summary = %q, want first", got)
	}
	if got := completed.Get("response.output.1.encrypted_content").String(); got != "sig_two" {
		t.Fatalf("output.1 encrypted_content = %q, want sig_two", got)
	}
	if got := completed.Get("response.output.1.summary.0.text").String(); got != "second" {
		t.Fatalf("output.1 summary = %q, want second", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_RedactedThinkingRoundTrips(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_red","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"OPAQUE"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var done gjson.Result
	var completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", nil, nil, chunk, &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.output_item.done" && data.Get("item.type").String() == "reasoning" {
				done = data
			}
			if event == "response.completed" {
				completed = data
			}
		}
	}

	if !done.Exists() {
		t.Fatal("expected reasoning output_item.done for redacted_thinking block")
	}
	if got := done.Get("item.encrypted_content").String(); got != "claude_redacted#OPAQUE" {
		t.Fatalf("item.encrypted_content = %q, want claude_redacted#OPAQUE", got)
	}
	if got := completed.Get("response.output.0.encrypted_content").String(); got != "claude_redacted#OPAQUE" {
		t.Fatalf("completed encrypted_content = %q, want claude_redacted#OPAQUE", got)
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_ErrorEventProducesErrorEnvelope(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_err","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("error.message").String(); got != "Internal server error" {
		t.Fatalf("error.message = %q, want Internal server error. Output: %s", got, string(out))
	}
	if got := root.Get("error.type").String(); got != "api_error" {
		t.Fatalf("error.type = %q, want api_error. Output: %s", got, string(out))
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_MaxTokensSetsIncomplete(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_trunc","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete. Output: %s", got, string(out))
	}
	if got := root.Get("incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason = %q, want max_output_tokens. Output: %s", got, string(out))
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_MultipleThinkingBlocksAllInOutput(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_multi","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_one"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"OPAQUE"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	}, "\n"))

	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", nil, nil, raw, nil)
	root := gjson.ParseBytes(out)

	if got := root.Get("output.#").Int(); got != 2 {
		t.Fatalf("output count = %d, want 2. Output: %s", got, string(out))
	}
	if got := root.Get("output.0.encrypted_content").String(); got != "sig_one" {
		t.Fatalf("output.0 encrypted_content = %q, want sig_one", got)
	}
	if got := root.Get("output.1.encrypted_content").String(); got != "claude_redacted#OPAQUE" {
		t.Fatalf("output.1 encrypted_content = %q, want claude_redacted#OPAQUE", got)
	}
}
