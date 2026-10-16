package translate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSourceFromPath(t *testing.T) {
	tests := map[string]Format{
		"/v1/chat/completions": OpenAI,
		"/v1/responses":        OpenAIResponses,
		"/v1/messages":         Claude,
		"/v1beta/models/gemini-2.5-flash:generateContent":       Gemini,
		"/v1beta/models/gemini-2.5-flash:streamGenerateContent": Gemini,
	}
	for path, want := range tests {
		got, ok := SourceFromPath(path)
		if !ok || got != want {
			t.Fatalf("SourceFromPath(%q) = %q, %v; want %q, true", path, got, ok, want)
		}
	}
}

func TestGeminiRequestPathHelpers(t *testing.T) {
	model, stream, ok := GeminiRequest("/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&key=secret")
	if !ok || model != "gemini-2.5-flash" || !stream {
		t.Fatalf("GeminiRequest() = %q, %v, %v", model, stream, ok)
	}
	if got := RequestModel("/v1beta/models/gemini-2.5-flash:generateContent", []byte(`{"model":"wrong"}`)); got != "gemini-2.5-flash" {
		t.Fatalf("RequestModel() = %q", got)
	}
	if !RequestStream("/v1beta/models/gemini-2.5-flash:streamGenerateContent", nil) {
		t.Fatal("RequestStream() should read native Gemini URL")
	}
	if got, err := TargetPathForRequest(Gemini, "/v1/responses", "gemini-2.5-flash", true); err != nil || got != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("TargetPathForRequest() = %q, %v", got, err)
	}
	query := TargetQuery(Gemini, Gemini, url.Values{"key": {"secret"}, "alt": {"sse"}, "trace": {"1"}}, true)
	if strings.Contains(query, "secret") || !strings.Contains(query, "alt=sse") || !strings.Contains(query, "trace=1") {
		t.Fatalf("TargetQuery() = %q", query)
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

func TestNewExchangeTranslatesGeminiRequestToOpenAI(t *testing.T) {
	original := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":16}}`)
	exchange, err := NewExchange(Gemini, OpenAI, "gemini-test", true, original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exchange.UpstreamRequest), `"messages"`) || !strings.Contains(string(exchange.UpstreamRequest), "hello") {
		t.Fatalf("Gemini request was not translated to OpenAI: %s", exchange.UpstreamRequest)
	}
	if !exchange.UpstreamStream {
		t.Fatal("streaming Gemini request should stay streaming upstream")
	}
}

func TestExchangeTranslatesOpenAIStreamBackToGemini(t *testing.T) {
	original := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	exchange, err := NewExchange(Gemini, OpenAI, "gemini-test", true, original)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := exchange.TranslateStream(context.Background(), []byte(`data: {"choices":[{"delta":{"content":"hello"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 || !strings.HasPrefix(string(outputs[0]), "data: ") || !strings.Contains(string(outputs[0]), "hello") {
		t.Fatalf("Gemini response framing = %q", outputs)
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

// Codex Desktop 通过 additional_tools input 项传工具定义，而不是顶层 tools 字段。
// Claude 方向的翻译器只读顶层 tools，不归一化的话工具会被静默丢弃，
// 模型收不到任何工具，只能自述意图后放弃。
const codexToolsViaAdditional = `{"model":"m","input":[` +
	`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"read_file",` +
	`"description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"list my desktop"}]}` +
	`],"stream":true}`

func TestNewExchangeKeepsAdditionalToolsForEveryTarget(t *testing.T) {
	for _, source := range []Format{Codex, OpenAIResponses} {
		for _, target := range []Format{Claude, OpenAI} {
			exchange, err := NewExchange(source, target, "m", true, []byte(codexToolsViaAdditional))
			if err != nil {
				t.Fatalf("%s -> %s: %v", source, target, err)
			}
			got := string(exchange.UpstreamRequest)
			if !strings.Contains(got, "read_file") {
				t.Fatalf("%s -> %s dropped the tool: %s", source, target, got)
			}
			// additional_tools 项必须移出 input，否则会被当成一条普通消息发给模型。
			if strings.Contains(got, "additional_tools") {
				t.Fatalf("%s -> %s leaked the additional_tools item: %s", source, target, got)
			}
			if !strings.Contains(got, "list my desktop") {
				t.Fatalf("%s -> %s lost the user message: %s", source, target, got)
			}
		}
	}
}

// 两个来源同时有工具时必须合并，不能相互覆盖。
func TestNewExchangeMergesBothToolSources(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"additional_tools","tools":[{"type":"function","name":"from_additional",` +
		`"parameters":{"type":"object","properties":{}}}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}` +
		`],"tools":[{"type":"function","name":"from_toplevel",` +
		`"parameters":{"type":"object","properties":{}}}],"stream":true}`
	exchange, err := NewExchange(Codex, Claude, "m", true, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := string(exchange.UpstreamRequest)
	for _, want := range []string{"from_toplevel", "from_additional"} {
		if !strings.Contains(got, want) {
			t.Fatalf("merged tools missing %q: %s", want, got)
		}
	}
}

func TestNewExchangeConvertsCodexCustomToolHistoryToClaude(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch",` +
		`"description":"Apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"edit the file"}]},` +
		`{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch"},` +
		`{"type":"custom_tool_call_output","call_id":"call_patch","output":"Done"}` +
		`],"stream":true}`

	exchange, err := NewExchange(Codex, Claude, "m", true, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := string(exchange.UpstreamRequest)
	for _, want := range []string{`"name":"apply_patch"`, `"input_schema":{"type":"object"`,
		`"type":"tool_use"`, `"input":{"input":"*** Begin Patch"}`, `"type":"tool_result"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("translated custom tool request missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "custom_tool_call") {
		t.Fatalf("Codex custom item leaked to Claude: %s", got)
	}
}

func TestExchangeRestoresClaudeCustomToolCallForCodex(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch",` +
		`"description":"Apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"edit"}]}` +
		`],"stream":true}`
	exchange, err := NewExchange(Codex, Claude, "m", true, []byte(body))
	if err != nil {
		t.Fatal(err)
	}

	lines := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_custom","usage":{"input_tokens":2}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_patch","name":"apply_patch","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** Begin Patch\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
	var all [][]byte
	for _, line := range lines {
		chunks, translateErr := exchange.TranslateStream(context.Background(), line)
		if translateErr != nil {
			t.Fatal(translateErr)
		}
		all = append(all, chunks...)
	}
	got := stringJoin(all)
	for _, want := range []string{"response.custom_tool_call_input.done", `"type":"custom_tool_call"`,
		`"id":"ctc_call_patch"`, `"input":"*** Begin Patch"`, "response.completed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("custom tool response missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "response.function_call_arguments") {
		t.Fatalf("custom tool was emitted as a function call: %s", got)
	}
}

func TestExchangeRestoresAdditionalToolNamespaceForCodex(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[` +
		`{"type":"function","name":"send_message","parameters":{"type":"object","properties":{}}}]}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"send"}]}` +
		`],"stream":true}`
	exchange, err := NewExchange(Codex, Claude, "m", true, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	_, err = exchange.TranslateStream(context.Background(),
		[]byte(`data: {"type":"message_start","message":{"id":"msg_namespace","usage":{"input_tokens":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := exchange.TranslateStream(context.Background(),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_send","name":"collaboration__send_message","input":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := stringJoin(chunks)
	for _, want := range []string{`"name":"send_message"`, `"namespace":"collaboration"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("namespace response missing %s: %s", want, got)
		}
	}
}

func TestExchangeNamespacedToolRoundTrip(t *testing.T) {
	for _, target := range []Format{Claude, OpenAI} {
		for _, kind := range []string{"custom", "function"} {
			t.Run(string(target)+"/"+kind, func(t *testing.T) {
				callType := "function_call"
				if kind == "custom" {
					callType = "custom_tool_call"
				}
				body := []byte(`{"model":"m","stream":true,"tools":[{"type":"namespace","name":"functions","tools":[{"type":"` + kind + `","name":"exec","parameters":{"type":"object","properties":{}}}]}],"input":[{"type":"` + callType + `","call_id":"call_old","name":"exec","namespace":"functions","arguments":"{}","input":"pwd"},{"type":"` + callType + `_output","call_id":"call_old","output":"ok"}]}`)
				exchange, err := NewExchange(Codex, target, "m", true, body)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(exchange.UpstreamRequest), `"name":"functions__exec"`) {
					t.Fatalf("missing qualified tool definition: %s", exchange.UpstreamRequest)
				}
				var request struct {
					Messages []struct {
						Content   json.RawMessage `json:"content"`
						ToolCalls []struct {
							Function struct {
								Name string `json:"name"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(exchange.UpstreamRequest, &request); err != nil {
					t.Fatal(err)
				}
				replayed := false
				for _, message := range request.Messages {
					for _, call := range message.ToolCalls {
						if call.Function.Name == "functions__exec" {
							replayed = true
						}
					}
					if strings.Contains(string(message.Content), `"name":"functions__exec"`) {
						replayed = true
					}
				}
				if !replayed {
					t.Errorf("history lost tool namespace: %s", exchange.UpstreamRequest)
				}

				lines := []string{
					`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":2}}}`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_exec","name":"functions__exec","input":{}}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"pwd\"}"}}`,
					`data: {"type":"content_block_stop","index":0}`,
					`data: {"type":"message_stop"}`,
				}
				if target == OpenAI {
					lines = []string{
						`data: {"id":"chat_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_exec","type":"function","function":{"name":"functions__exec","arguments":"{\"input\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`,
						`data: [DONE]`,
					}
				}
				itemCount := 0
				for _, line := range lines {
					chunks, err := exchange.TranslateStream(context.Background(), []byte(line))
					if err != nil {
						t.Fatal(err)
					}
					for _, chunk := range chunks {
						for _, eventLine := range strings.Split(string(chunk), "\n") {
							if !strings.HasPrefix(eventLine, "data:") {
								continue
							}
							var event struct {
								Item     json.RawMessage `json:"item"`
								Response struct {
									Output []json.RawMessage `json:"output"`
								} `json:"response"`
							}
							if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(eventLine, "data:"))), &event); err != nil {
								t.Fatal(err)
							}
							items := event.Response.Output
							if len(event.Item) > 0 {
								items = append(items, event.Item)
							}
							for _, raw := range items {
								var item struct{ Type, Name, Namespace, Input string }
								if err := json.Unmarshal(raw, &item); err != nil {
									t.Fatal(err)
								}
								if item.Type != "custom_tool_call" && item.Type != "function_call" {
									continue
								}
								itemCount++
								if item.Name != "exec" || item.Namespace != "functions" {
									t.Errorf("tool identity changed: %s", raw)
								}
							}
						}
					}
				}
				if itemCount != 3 {
					t.Fatalf("got %d tool items, want added, done and completed", itemCount)
				}
			})
		}
	}
}

func TestExchangeNamespacedCustomToolNonStream(t *testing.T) {
	for _, target := range []Format{Claude, OpenAI} {
		t.Run(string(target), func(t *testing.T) {
			body := []byte(`{"model":"m","stream":false,"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},{"role":"user","content":"run pwd"}]}`)
			exchange, err := NewExchange(Codex, target, "m", false, body)
			if err != nil {
				t.Fatal(err)
			}
			upstreamBody := []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_exec\",\"name\":\"functions__exec\",\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"input\\\":\\\"pwd\\\"}\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			if target == OpenAI {
				upstreamBody = []byte(`{"id":"chat_1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_exec","type":"function","function":{"name":"functions__exec","arguments":"{\"input\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`)
			}
			response, err := exchange.TranslateNonStream(context.Background(), upstreamBody)
			if err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Output []struct{ Type, Name, Namespace, Input string }
			}
			if err := json.Unmarshal(response, &parsed); err != nil {
				t.Fatal(err)
			}
			if len(parsed.Output) != 1 {
				t.Fatalf("expected one tool call: %s", response)
			}
			item := parsed.Output[0]
			if item.Type != "custom_tool_call" || item.Name != "exec" || item.Namespace != "functions" || item.Input != "pwd" {
				t.Fatalf("tool identity or input changed: %s", response)
			}
		})
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
