package responses

import (
	"encoding/base64"
	"strings"
	"testing"

	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertOpenAIResponsesRequestToClaude_SanitizesToolCallIDsForClaude(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"input": [
			{
				"type": "function_call",
				"call_id": "call.with space:1",
				"name": "Read",
				"arguments": "{\"path\":\"README.md\"}"
			},
			{
				"type": "function_call_output",
				"call_id": "call.with space:1",
				"output": "ok"
			}
		]
	}`

	result := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	toolUseID := resultJSON.Get("messages.0.content.0.id").String()
	toolResultID := resultJSON.Get("messages.1.content.0.tool_use_id").String()

	if toolUseID != "call_with_space_1" {
		t.Fatalf("tool_use id = %q, want %q", toolUseID, "call_with_space_1")
	}
	if toolResultID != toolUseID {
		t.Fatalf("tool_result tool_use_id = %q, want same sanitized id %q", toolResultID, toolUseID)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ReasoningItemToThinkingBlock(t *testing.T) {
	rawSignature, expectedSignature := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + rawSignature + `",
				"summary":[{"type":"summary_text","text":"internal reasoning"}]
			},
			{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"output_text","text":"visible answer"}]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	assistant := root.Get("messages.0")
	if got := assistant.Get("role").String(); got != "assistant" {
		t.Fatalf("first message role = %q, want assistant. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.signature").String(); got != expectedSignature {
		t.Fatalf("thinking signature = %q, want %q", got, expectedSignature)
	}
	if got := assistant.Get("content.0.thinking").String(); got != "internal reasoning" {
		t.Fatalf("thinking text = %q, want internal reasoning", got)
	}
	if got := assistant.Get("content.1.type").String(); got != "text" {
		t.Fatalf("second content type = %q, want text. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.1.text").String(); got != "visible answer" {
		t.Fatalf("assistant text = %q, want visible answer", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SignatureOnlyReasoningFlushesBeforeUser(t *testing.T) {
	rawSignature, expectedSignature := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + rawSignature + `",
				"summary":[]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	thinking := root.Get("messages.0.content.0")
	if got := thinking.Get("type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := thinking.Get("signature").String(); got != expectedSignature {
		t.Fatalf("thinking signature = %q, want %q", got, expectedSignature)
	}
	if got := thinking.Get("thinking").String(); got != "" {
		t.Fatalf("thinking text = %q, want empty", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DropsIncompatibleReasoningSignature(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + testGPTResponsesReasoningSignature() + `",
				"summary":[{"type":"summary_text","text":"must not become Claude thinking"}]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)

	if gjson.GetBytes(out, "messages.0.content.0.type").String() == "thinking" {
		t.Fatalf("GPT encrypted_content should not become Claude thinking. Output: %s", string(out))
	}
	if gjson.GetBytes(out, "messages.0.content.0.signature").Exists() {
		t.Fatalf("incompatible signature should not be forwarded. Output: %s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Fatalf("first message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_FunctionCallOutputPreservesInputImage(t *testing.T) {
	const imageB64 = "iVBORw0KGgo="
	dataURL := "data:image/png;base64," + imageB64
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"function_call",
				"call_id":"call_view_image_1",
				"name":"view_image",
				"arguments":"{}"
			},
			{
				"type":"function_call_output",
				"call_id":"call_view_image_1",
				"output":[
					{
						"type":"input_image",
						"image_url":"` + dataURL + `",
						"detail":"high"
					}
				]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	toolResult := root.Get("messages.1.content.0")
	if got := toolResult.Get("type").String(); got != "tool_result" {
		t.Fatalf("tool_result type = %q, want tool_result. Output: %s", got, string(out))
	}
	if got := toolResult.Get("content.0.type").String(); got != "image" {
		t.Fatalf("tool_result content block type = %q, want image. Output: %s", got, string(out))
	}
	if got := toolResult.Get("content.0.source.media_type").String(); got != "image/png" {
		t.Fatalf("image media_type = %q, want image/png. Output: %s", got, string(out))
	}
	if got := toolResult.Get("content.0.source.data").String(); got != imageB64 {
		t.Fatalf("image data = %q, want raw base64 without data URL prefix", got)
	}
	if strings.Contains(toolResult.Get("content").Raw, "data:image") {
		t.Fatalf("tool_result content must not embed data URL as text. Output: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_KeepsToolUseAdjacentToToolResult(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"function_call",
				"call_id":"call_00_awGuheXs4aRbtedNK8LE3743",
				"name":"js",
				"arguments":"{\"code\":\"nodeRepl.write('ok')\",\"title\":\"List Obsidian vault contents\"}"
			},
			{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"output_text","text":"I'll check your Obsidian vault for articles."}]
			},
			{
				"type":"function_call_output",
				"call_id":"call_00_awGuheXs4aRbtedNK8LE3743",
				"output":"Wall time: 0.1963 seconds\nOutput:\n[{\"type\":\"text\",\"text\":\"\"}]"
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.0.role").String(); got != "assistant" {
		t.Fatalf("first message role = %q, want assistant. Output: %s", got, string(out))
	}
	if got := root.Get("messages.0.content").String(); got != "I'll check your Obsidian vault for articles." {
		t.Fatalf("first message content = %q, want assistant text. Output: %s", got, string(out))
	}
	if got := root.Get("messages.1.content.0.type").String(); got != "tool_use" {
		t.Fatalf("second message first content type = %q, want tool_use. Output: %s", got, string(out))
	}
	if got := root.Get("messages.1.content.0.id").String(); got != "call_00_awGuheXs4aRbtedNK8LE3743" {
		t.Fatalf("tool_use id = %q, want call_00_awGuheXs4aRbtedNK8LE3743. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.content.0.type").String(); got != "tool_result" {
		t.Fatalf("third message first content type = %q, want tool_result. Output: %s", got, string(out))
	}
	if got := root.Get("messages.2.content.0.tool_use_id").String(); got != "call_00_awGuheXs4aRbtedNK8LE3743" {
		t.Fatalf("tool_result id = %q, want call_00_awGuheXs4aRbtedNK8LE3743. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ConvertsApplyPatchCustomTool(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[
			{
				"type":"custom",
				"name":"apply_patch",
				"description":"Use the apply_patch tool to edit files.",
				"format":{"type":"grammar","syntax":"lark","definition":"start: patch"}
			},
			{
				"type":"function",
				"name":"exec_command",
				"description":"Runs a command.",
				"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("tools.#").Int(); got != 2 {
		t.Fatalf("tools count = %d, want 2. Output: %s", got, string(out))
	}
	custom := root.Get("tools.#(name==\"apply_patch\")")
	if !custom.Exists() {
		t.Fatalf("apply_patch custom tool was dropped. Output: %s", string(out))
	}
	if got := custom.Get("input_schema.properties.input.type").String(); got != "string" {
		t.Fatalf("apply_patch input type = %q, want string. Output: %s", got, string(out))
	}
	if got := root.Get("tools.#(name==\"exec_command\").name").String(); got != "exec_command" {
		t.Fatalf("exec_command tool missing. Output: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_UsesTopLevelSystem(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"instructions":"base instructions",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"developer rules"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)
	if got := root.Get("system").String(); got != "base instructions\n\ndeveloper rules" {
		t.Fatalf("system = %q, want merged instructions. Output: %s", got, string(out))
	}
	if got := root.Get("messages.#").Int(); got != 1 {
		t.Fatalf("messages count = %d, want 1. Output: %s", got, string(out))
	}
	if got := root.Get("messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MapsCustomToolChoice(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"edit"}]}],
		"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch"}],
		"tool_choice":{"type":"custom","name":"apply_patch"},
		"parallel_tool_calls":false
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)
	if got := root.Get("tool_choice.type").String(); got != "tool" {
		t.Fatalf("tool_choice.type = %q, want tool. Output: %s", got, string(out))
	}
	if got := root.Get("tool_choice.name").String(); got != "apply_patch" {
		t.Fatalf("tool_choice.name = %q, want apply_patch. Output: %s", got, string(out))
	}
	if !root.Get("tool_choice.disable_parallel_tool_use").Bool() {
		t.Fatalf("disable_parallel_tool_use was not preserved. Output: %s", string(out))
	}
}

func testClaudeResponsesThinkingSignature(t *testing.T) (string, string) {
	t.Helper()
	channelBlock := []byte{}
	channelBlock = protowire.AppendTag(channelBlock, 1, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 12)
	channelBlock = protowire.AppendTag(channelBlock, 2, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 2)
	channelBlock = protowire.AppendTag(channelBlock, 6, protowire.BytesType)
	channelBlock = protowire.AppendString(channelBlock, "claude-sonnet-4-6")

	container := []byte{}
	container = protowire.AppendTag(container, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channelBlock)

	payload := []byte{}
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	payload = protowire.AppendTag(payload, 3, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)

	rawSignature := base64.StdEncoding.EncodeToString(payload)
	normalized, ok := sigcompat.CompatibleSignatureForProvider(sigcompat.SignatureProviderClaude, rawSignature)
	if !ok {
		t.Fatal("test Claude signature should be compatible")
	}
	return rawSignature, normalized
}

func testGPTResponsesReasoningSignature() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	payload[8] = 1
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.URLEncoding.EncodeToString(payload)
}

func TestConvertOpenAIResponsesRequestToClaude_ClampsThinkingBudgetToExplicitMaxTokens(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"max_output_tokens":4096,
		"reasoning":{"effort":"xhigh"},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	maxTokens := root.Get("max_tokens").Int()
	if maxTokens != 4096 {
		t.Fatalf("max_tokens = %d, want explicit 4096 preserved. Output: %s", maxTokens, string(out))
	}
	budget := root.Get("thinking.budget_tokens").Int()
	if budget >= maxTokens {
		t.Fatalf("budget_tokens = %d must be < max_tokens %d. Output: %s", budget, maxTokens, string(out))
	}
	if budget < 1024 {
		t.Fatalf("budget_tokens = %d, want >= 1024. Output: %s", budget, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_RaisesDefaultMaxTokensForLargeBudget(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"reasoning":{"effort":"xhigh"},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	budget := root.Get("thinking.budget_tokens").Int()
	maxTokens := root.Get("max_tokens").Int()
	if budget != 32768 {
		t.Fatalf("budget_tokens = %d, want xhigh budget 32768. Output: %s", budget, string(out))
	}
	if maxTokens <= budget {
		t.Fatalf("default max_tokens = %d must be raised above budget %d. Output: %s", maxTokens, budget, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DisablesThinkingWhenNoBudgetRoom(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"max_output_tokens":1500,
		"reasoning":{"effort":"medium"},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if root.Get("thinking").Exists() {
		t.Fatalf("thinking should be removed when clamped budget < 1024. Output: %s", string(out))
	}
	if got := root.Get("max_tokens").Int(); got != 1500 {
		t.Fatalf("max_tokens = %d, want explicit 1500 preserved. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MergesParallelToolCallsIntoOneAssistantMessage(t *testing.T) {
	rawSignature, _ := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"reasoning","encrypted_content":"` + rawSignature + `","summary":[{"type":"summary_text","text":"plan"}]},
			{"type":"function_call","call_id":"call_a","name":"read","arguments":"{\"path\":\"a\"}"},
			{"type":"function_call","call_id":"call_b","name":"read","arguments":"{\"path\":\"b\"}"},
			{"type":"function_call_output","call_id":"call_a","output":"content a"},
			{"type":"function_call_output","call_id":"call_b","output":"content b"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.#").Int(); got != 2 {
		t.Fatalf("messages count = %d, want 2 (one assistant, one user). Output: %s", got, string(out))
	}
	assistant := root.Get("messages.0")
	if got := assistant.Get("role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want assistant. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.type").String(); got != "thinking" {
		t.Fatalf("assistant content.0.type = %q, want thinking first. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.1.id").String(); got != "call_a" {
		t.Fatalf("assistant content.1.id = %q, want call_a. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.2.id").String(); got != "call_b" {
		t.Fatalf("assistant content.2.id = %q, want call_b. Output: %s", got, string(out))
	}
	user := root.Get("messages.1")
	if got := user.Get("role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user. Output: %s", got, string(out))
	}
	if got := user.Get("content.0.tool_use_id").String(); got != "call_a" {
		t.Fatalf("user content.0.tool_use_id = %q, want call_a. Output: %s", got, string(out))
	}
	if got := user.Get("content.1.tool_use_id").String(); got != "call_b" {
		t.Fatalf("user content.1.tool_use_id = %q, want call_b. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SynthesizesResultForOrphanFunctionCall(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call","call_id":"call_orphan","name":"read","arguments":"{}"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"never mind"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.0.content.0.type").String(); got != "tool_use" {
		t.Fatalf("messages.0 should carry the tool_use. Output: %s", string(out))
	}
	synth := root.Get("messages.1.content.0")
	if got := synth.Get("type").String(); got != "tool_result" {
		t.Fatalf("messages.1.content.0.type = %q, want synthesized tool_result. Output: %s", got, string(out))
	}
	if got := synth.Get("tool_use_id").String(); got != "call_orphan" {
		t.Fatalf("synthesized tool_use_id = %q, want call_orphan. Output: %s", got, string(out))
	}
	if !synth.Get("is_error").Bool() {
		t.Fatalf("synthesized tool_result should be marked is_error. Output: %s", string(out))
	}
	if got := root.Get("messages.2.role").String(); got != "user" {
		t.Fatalf("messages.2.role = %q, want trailing user text. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_FlushesResultsBeforeAssistantText(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"content"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done reading"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"thanks"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.0.content.0.type").String(); got != "tool_use" {
		t.Fatalf("messages.0 = %q, want tool_use. Output: %s", got, string(out))
	}
	if got := root.Get("messages.1.content.0.type").String(); got != "tool_result" {
		t.Fatalf("tool_result must directly follow the tool_use message, messages.1 = %s. Output: %s", root.Get("messages.1").Raw, string(out))
	}
	if got := root.Get("messages.2.role").String(); got != "assistant" {
		t.Fatalf("messages.2.role = %q, want assistant text after the result. Output: %s", got, string(out))
	}
	if got := root.Get("messages.3.role").String(); got != "user" {
		t.Fatalf("messages.3.role = %q, want trailing user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_InterleavedParallelOutputsKeepOneRound(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},
			{"type":"function_call","call_id":"call_2","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"one"},
			{"type":"function_call","call_id":"call_3","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_2","output":"two"},
			{"type":"function_call_output","call_id":"call_3","output":"three"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if strings.Contains(string(out), "is_error") {
		t.Fatalf("calls whose outputs arrive later must not get synthetic error results. Output: %s", string(out))
	}
	// Every call must yield exactly one tool_result, and all results must sit
	// after the last tool_use message so Claude's same-role merge keeps the
	// round shape tool_use* -> tool_result*.
	counts := map[string]int{}
	lastToolUseIdx, firstResultIdx := -1, -1
	root.Get("messages").ForEach(func(key, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "tool_use":
				lastToolUseIdx = int(key.Int())
			case "tool_result":
				counts[part.Get("tool_use_id").String()]++
				if firstResultIdx < 0 {
					firstResultIdx = int(key.Int())
				}
			}
			return true
		})
		return true
	})
	for _, id := range []string{"call_1", "call_2", "call_3"} {
		if counts[id] != 1 {
			t.Fatalf("tool_result for %s appears %d times, want exactly 1. Output: %s", id, counts[id], string(out))
		}
	}
	if firstResultIdx <= lastToolUseIdx {
		t.Fatalf("tool_results (first at messages.%d) must follow the last tool_use message (messages.%d). Output: %s", firstResultIdx, lastToolUseIdx, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_UserMessageMidBatchDoesNotDuplicateToolResult(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"tool_a","arguments":"{}"},
			{"type":"function_call","call_id":"call_2","name":"tool_b","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"res1"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"wait"}]},
			{"type":"function_call_output","call_id":"call_2","output":"res2"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)

	counts := map[string]int{}
	toolUseIDs := map[string]struct{}{}
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "tool_use":
				toolUseIDs[part.Get("id").String()] = struct{}{}
			case "tool_result":
				counts[part.Get("tool_use_id").String()]++
			}
			return true
		})
		return true
	})
	for id, n := range counts {
		if n != 1 {
			t.Fatalf("tool_result for %s appears %d times, want exactly 1. Output: %s", id, n, string(out))
		}
		if _, ok := toolUseIDs[id]; !ok {
			t.Fatalf("tool_result references unknown tool_use_id %s. Output: %s", id, string(out))
		}
	}
	for id := range toolUseIDs {
		if counts[id] != 1 {
			t.Fatalf("tool_use %s has %d results, want exactly 1. Output: %s", id, counts[id], string(out))
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DropsToolResultWithoutMatchingToolUse(t *testing.T) {
	// A compacted history can keep a function_call_output whose function_call was
	// trimmed away. Claude rejects a tool_result whose tool_use_id it never saw.
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call_output","call_id":"call_gone","output":"res"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)

	if strings.Contains(string(out), "tool_result") {
		t.Fatalf("orphan tool_result without a tool_use must be dropped. Output: %s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("remaining user message = %q, want hi. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MultiRoundToolLoopKeepsThinkingFirst(t *testing.T) {
	rawSignature, _ := testClaudeResponsesThinkingSignature(t)
	reasoning := `{"type":"reasoning","encrypted_content":"` + rawSignature + `","summary":[{"type":"summary_text","text":"plan"}]}`
	raw := []byte(`{
		"model":"claude-test",
		"reasoning":{"effort":"medium"},
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			` + reasoning + `,
			{"type":"function_call","call_id":"call_1","name":"tool_a","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"res1"},
			` + reasoning + `,
			{"type":"function_call","call_id":"call_2","name":"tool_b","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_2","output":"res2"},
			` + reasoning + `,
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"thanks"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)

	for _, turn := range mergeClaudeSameRoleTurns(t, out) {
		sawNonThinking := false
		for _, blockType := range turn.blocks {
			switch blockType {
			case "thinking", "redacted_thinking":
				if sawNonThinking {
					t.Fatalf("thinking must lead the merged %s turn, got %v. Output: %s", turn.role, turn.blocks, string(out))
				}
			default:
				sawNonThinking = true
			}
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaude_AssistantTextMidParallelBatchKeepsRound(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},
			{"type":"function_call","call_id":"call_2","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"one"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"halfway"}]},
			{"type":"function_call_output","call_id":"call_2","output":"two"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if strings.Contains(string(out), "is_error") {
		t.Fatalf("call_2's real output arrives later; no synthetic error result allowed. Output: %s", string(out))
	}
	counts := map[string]int{}
	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "tool_result" {
				counts[part.Get("tool_use_id").String()]++
			}
			return true
		})
		return true
	})
	for _, id := range []string{"call_1", "call_2"} {
		if counts[id] != 1 {
			t.Fatalf("tool_result for %s appears %d times, want exactly 1. Output: %s", id, counts[id], string(out))
		}
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ReasoningBetweenParallelOutputsStaysAheadOfToolUse(t *testing.T) {
	rawSignature, _ := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"reasoning":{"effort":"medium"},
		"input":[
			{"type":"function_call","call_id":"call_1","name":"tool_a","arguments":"{}"},
			{"type":"function_call","call_id":"call_2","name":"tool_b","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"res1"},
			{"type":"reasoning","encrypted_content":"` + rawSignature + `","summary":[{"type":"summary_text","text":"mid"}]},
			{"type":"function_call_output","call_id":"call_2","output":"res2"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)

	// Claude merges consecutive same-role messages, and with thinking enabled a
	// thinking block may not follow a tool_use inside the merged turn.
	for _, turn := range mergeClaudeSameRoleTurns(t, out) {
		sawToolUse := false
		for _, blockType := range turn.blocks {
			switch blockType {
			case "tool_use":
				sawToolUse = true
			case "thinking", "redacted_thinking":
				if sawToolUse {
					t.Fatalf("thinking block follows tool_use in merged %s turn %v. Output: %s", turn.role, turn.blocks, string(out))
				}
			}
		}
	}
}

type claudeMergedTurn struct {
	role   string
	blocks []string
}

// mergeClaudeSameRoleTurns mirrors Anthropic's merging of consecutive same-role
// messages so tests can assert on the block order the API actually validates.
func mergeClaudeSameRoleTurns(t *testing.T, out []byte) []claudeMergedTurn {
	t.Helper()
	var turns []claudeMergedTurn
	gjson.GetBytes(out, "messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		var blocks []string
		if content := msg.Get("content"); content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				blocks = append(blocks, block.Get("type").String())
				return true
			})
		} else {
			blocks = append(blocks, "text")
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return true
		}
		turns = append(turns, claudeMergedTurn{role: role, blocks: blocks})
		return true
	})
	return turns
}

func TestConvertOpenAIResponsesRequestToClaude_TrailingReasoningAfterOutputStaysBehindResults(t *testing.T) {
	rawSignature, _ := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"content"},
			{"type":"reasoning","encrypted_content":"` + rawSignature + `","summary":[{"type":"summary_text","text":"tail"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if got := root.Get("messages.1.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.1 must be the tool_result, got %s. Output: %s", root.Get("messages.1").Raw, string(out))
	}
	if got := root.Get("messages.2.content.0.type").String(); got != "thinking" {
		t.Fatalf("trailing reasoning should land after the results, messages.2 = %s. Output: %s", root.Get("messages.2").Raw, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DroppedThinkingAlsoDropsHistoryThinkingBlocks(t *testing.T) {
	rawSignature, _ := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"max_output_tokens":1500,
		"reasoning":{"effort":"medium"},
		"input":[
			{"type":"reasoning","encrypted_content":"` + rawSignature + `","summary":[{"type":"summary_text","text":"plan"}]},
			{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	if root.Get("thinking").Exists() {
		t.Fatalf("thinking config should be removed. Output: %s", string(out))
	}
	if got := root.Get("messages.0.content.0.type").String(); got != "tool_use" {
		t.Fatalf("history thinking blocks must be dropped when thinking is disabled, messages.0.content.0 = %s. Output: %s", root.Get("messages.0.content.0").Raw, string(out))
	}
	if strings.Contains(string(out), `"type":"thinking"`) || strings.Contains(string(out), `"redacted_thinking"`) {
		t.Fatalf("no thinking-family blocks may remain when thinking is disabled. Output: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MapsToolChoiceNone(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}}],
		"tool_choice":"none"
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "none" {
		t.Fatalf("tool_choice.type = %q, want none. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_RestoresRedactedThinking(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{"type":"reasoning","encrypted_content":"claude_redacted#OPAQUEDATA","summary":[]},
			{"type":"function_call","call_id":"call_r","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_r","output":"ok"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	block := root.Get("messages.0.content.0")
	if got := block.Get("type").String(); got != "redacted_thinking" {
		t.Fatalf("content.0.type = %q, want redacted_thinking. Output: %s", got, string(out))
	}
	if got := block.Get("data").String(); got != "OPAQUEDATA" {
		t.Fatalf("redacted data = %q, want OPAQUEDATA. Output: %s", got, string(out))
	}
	if got := root.Get("messages.0.content.1.type").String(); got != "tool_use" {
		t.Fatalf("content.1.type = %q, want tool_use in same message. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_PreservesContentPartCacheControl(t *testing.T) {
	inputJSON := `{
		"model": "gpt-4.1",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "cached prefix", "cache_control": {"type": "ephemeral"}},
					{"type": "input_text", "text": "fresh question"}
				]
			}
		]
	}`

	result := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-5", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)

	content := resultJSON.Get("messages.0.content")
	if !content.IsArray() {
		t.Fatalf("expected content array when cache_control is present, got %s", result)
	}
	if got := content.Get("0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("content.0.cache_control.type = %q, want ephemeral. Output: %s", got, result)
	}
	if content.Get("1.cache_control").Exists() {
		t.Fatalf("content.1 should not have cache_control. Output: %s", result)
	}
}
