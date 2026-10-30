package routing

import (
	"encoding/json"
	"strings"

	"github.com/mirainya/muxapi/internal/tokenusage"
)

// EstimateOutboundInput measures the final translated payload, independently
// of the client-side features used to choose an upstream. Framing and tool
// serialization remain approximations even with a known tokenizer vocabulary.
func EstimateOutboundInput(body []byte, model, protocol string) tokenusage.Estimate {
	result := tokenusage.Estimate{Model: model, Protocol: tokenusage.Protocol(protocol), Source: "local_tiktoken", Covered: true}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		result.Reason = "invalid_request"
		result.Covered = false
		return result
	}
	if result.Protocol == "" {
		result.Covered, result.Reason = false, "unknown_protocol"
		return result
	}
	lower := strings.ToLower(model)
	known := strings.HasPrefix(lower, "gpt-5") || strings.HasPrefix(lower, "gpt-4") || strings.HasPrefix(lower, "gpt-3.5")
	if !known || result.Protocol == "claude" || result.Protocol == "gemini" {
		result.Source, result.Covered, result.Reason = "local_tiktoken_fallback", false, "unverified_tokenizer"
	}
	meter := inputMeter{}
	for _, key := range []string{"previous_response_id", "conversation", "prompt", "context_management"} {
		if value := root[key]; value != nil && value != "" {
			meter.reason = "server_context"
		}
	}
	meter.text(root["instructions"])
	switch result.Protocol {
	case "responses":
		meter.items(root["input"])
	case "openai", "claude":
		meter.text(root["system"])
		meter.items(root["messages"])
	default:
		meter.reason = "unverified_tokenizer"
	}
	// Inspect only real tool definitions, never arbitrary user schema keys.
	for _, key := range []string{"tools", "functions"} {
		if tools, ok := root[key].([]any); ok {
			for _, raw := range tools {
				tool, ok := raw.(map[string]any)
				if !ok {
					meter.reason = "unsupported_tools"
					continue
				}
				typ, _ := tool["type"].(string)
				if typ != "" && typ != "function" && typ != "custom" {
					meter.reason = "server_tools"
					continue
				}
				meter.structured(tool)
			}
		} else if root[key] != nil {
			meter.reason = "unsupported_tools"
		}
	}
	for _, key := range []string{"tool_choice", "response_format", "text"} {
		meter.structured(root[key])
	}
	if meter.reason != "" {
		result.Covered, result.Reason = false, meter.reason
	}
	if !meter.hasInput && result.Covered {
		result.Covered, result.Reason = false, "unsupported_content"
	}
	codec, err := codexTokenizer(model)
	if err != nil {
		result.Covered, result.Reason = false, "tokenizer_unavailable"
		return result
	}
	count, err := codec.Count(strings.Join(meter.parts, "\n"))
	if err != nil {
		result.Covered, result.Reason = false, "tokenizer_unavailable"
		return result
	}
	result.Tokens = int64(count) + meter.framing
	return result
}

type inputMeter struct {
	parts    []string
	framing  int64
	reason   string
	hasInput bool
}

func (m *inputMeter) structured(value any) {
	if value == nil {
		return
	}
	if text, ok := value.(string); ok {
		m.parts = append(m.parts, text)
		return
	}
	raw, err := json.Marshal(value)
	if err == nil {
		m.parts = append(m.parts, string(raw))
	}
}

func (m *inputMeter) text(value any) {
	switch item := value.(type) {
	case nil:
	case string:
		m.parts = append(m.parts, item)
	case []any:
		for _, part := range item {
			m.text(part)
		}
	case map[string]any:
		typ, _ := item["type"].(string)
		switch typ {
		case "text", "input_text", "output_text", "summary_text":
			m.structured(item["text"])
		case "refusal":
			m.structured(item["refusal"])
		default:
			m.reason = "unsupported_content"
		}
	default:
		m.reason = "unsupported_content"
	}
}

func (m *inputMeter) items(value any) {
	if value == nil {
		m.reason = "unsupported_content"
		return
	}
	if text, ok := value.(string); ok {
		m.hasInput = true
		m.parts = append(m.parts, text)
		return
	}
	items, ok := value.([]any)
	if !ok {
		m.reason = "unsupported_content"
		return
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			m.reason = "unsupported_content"
			continue
		}
		m.hasInput = true
		typ, _ := item["type"].(string)
		switch typ {
		case "message", "":
			m.text(item["content"])
			for _, key := range []string{"role", "name", "tool_call_id", "tool_calls", "function_call"} {
				m.structured(item[key])
			}
			for _, key := range []string{"audio", "image_url", "input_audio"} {
				if item[key] != nil {
					m.reason = "unsupported_content"
				}
			}
			m.framing += 3
		case "function_call", "custom_tool_call":
			for _, key := range []string{"name", "arguments", "input", "call_id"} {
				m.structured(item[key])
			}
		case "function_call_output", "custom_tool_call_output":
			m.text(item["output"])
			m.structured(item["call_id"])
		case "reasoning", "compaction", "compaction_trigger", "item_reference":
			m.reason = "server_context"
		default:
			m.reason = "unsupported_content"
		}
		if item["encrypted_content"] != nil {
			m.reason = "server_context"
		}
	}
	m.framing += 3
}
