package routing

import (
	"strings"
	"testing"
)

func TestOutboundEstimateIncludesToolsAndActualModel(t *testing.T) {
	base := `{"model":"gpt-5","input":[{"role":"user","content":"Hello"}]}`
	plain := EstimateOutboundInput([]byte(base), "gpt-5", "responses")
	withTools := strings.TrimSuffix(base, "}") + `,"tools":[{"type":"function","name":"lookup","description":"Find an address in the customer directory","parameters":{"type":"object","properties":{"conversation":{"type":"string"},"image":{"type":"string"}}}}]}`
	tools := EstimateOutboundInput([]byte(withTools), "gpt-5", "responses")
	if !plain.Covered || !tools.Covered || tools.Tokens <= plain.Tokens {
		t.Fatalf("tools missing or schema keys misclassified: plain=%+v tools=%+v", plain, tools)
	}
	chat := `{"messages":[{"role":"user","content":"Hello"}]}`
	chatTools := strings.TrimSuffix(chat, "}") + `,"tools":[{"type":"function","function":{"name":"lookup","description":"Look up a customer record","parameters":{"type":"object"}}}]}`
	a, b := EstimateOutboundInput([]byte(chat), "gpt-4o", "openai"), EstimateOutboundInput([]byte(chatTools), "gpt-4o", "openai")
	if !a.Covered || !b.Covered || b.Tokens <= a.Tokens {
		t.Fatalf("chat tool definition omitted: %+v %+v", a, b)
	}
	mapped := EstimateOutboundInput([]byte(chat), "claude-sonnet-4", "claude")
	if mapped.Covered || mapped.Model != "claude-sonnet-4" || mapped.Protocol != "claude" || mapped.Reason != "unverified_tokenizer" {
		t.Fatalf("mapped model ignored: %+v", mapped)
	}
}

func TestOutboundEstimateRejectsUncountedInput(t *testing.T) {
	for _, tc := range []struct{ body, reason string }{
		{`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`, "unsupported_content"},
		{`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, "server_context"},
		{`{"input":"hello","previous_response_id":"resp_1"}`, "server_context"},
		{`{"input":"hello","tools":[{"type":"web_search"}]}`, "server_tools"},
		{`{"input":[{"type":"new_item_type","input":"hello"}]}`, "unsupported_content"},
		{`{"input":"hello","prompt":{"id":"pmpt_1"}}`, "server_context"},
	} {
		m := EstimateOutboundInput([]byte(tc.body), "gpt-5", "responses")
		if m.Covered || m.Reason != tc.reason {
			t.Fatalf("uncounted content accepted: %s: %+v", tc.body, m)
		}
	}
	if m := EstimateOutboundInput([]byte(`{"input":"hello"}`), "unknown-model", "responses"); m.Covered || m.Source != "local_tiktoken_fallback" {
		t.Fatalf("unknown model accepted: %+v", m)
	}
}
