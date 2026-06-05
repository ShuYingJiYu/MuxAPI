package monitor

import (
	"encoding/json"
	"testing"

	"github.com/mirainya/muxapi/internal/store"
)

func TestBuildProbeBody(t *testing.T) {
	// 默认：空文本→"hi"，0 tokens→1，非流式不带 stream
	b := buildProbeBody(&store.Monitor{Model: "gpt-4o"})
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("默认体应为合法 JSON: %v", err)
	}
	if d["model"] != "gpt-4o" || d["max_tokens"].(float64) != 1 {
		t.Fatalf("默认 model/max_tokens 错: %+v", d)
	}
	if _, ok := d["stream"]; ok {
		t.Fatal("非流式不应带 stream 字段")
	}
	msgs := d["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("空文本应默认 hi: %+v", msgs)
	}

	// 自定义：文本含引号(验证 json 编码不破坏)、自定义 tokens、流式
	b = buildProbeBody(&store.Monitor{Model: "m", ProbeText: `say "ok"`, MaxTokens: 5, Stream: true})
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("含引号文本应仍是合法 JSON: %v", err)
	}
	if d["max_tokens"].(float64) != 5 || d["stream"] != true {
		t.Fatalf("自定义 tokens/stream 未生效: %+v", d)
	}
	if d["messages"].([]any)[0].(map[string]any)["content"] != `say "ok"` {
		t.Fatalf("自定义文本未保留: %+v", d)
	}
}
