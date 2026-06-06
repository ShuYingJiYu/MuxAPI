package monitor

import (
	"encoding/json"
	"testing"

	"github.com/mirainya/muxapi/internal/store"
)

// fakeBreaker 捕获 observe 喂给熔断器的 (id, model, ok) 三元组。
type fakeBreaker struct {
	gotID    int64
	gotModel string
	gotOK    bool
	called   bool
}

func (f *fakeBreaker) ObserveProbe(id int64, model string, ok bool, latencyMs int64) {
	f.gotID, f.gotModel, f.gotOK, f.called = id, model, ok, true
}

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

// 验证 observe 用【熔断器口径】喂熔断器，且与看板 classify 口径分离：
//   - 2xx → ok=true，模型级
//   - 429 → 看板算降级，但熔断器算【失败】，模型级（不连累整上游）
//   - 401 → 凭证类，熔断器记上游级(scope="")，所有模型连坐
//   - 网络错误 → 模型级失败
func TestObserveBreakerScope(t *testing.T) {
	m := &store.Monitor{UpstreamID: 7, Model: "gpt-x"}
	cases := []struct {
		name      string
		hasResp   bool
		code      int
		wantOK    bool
		wantModel string
	}{
		{"2xx成功", true, 200, true, "gpt-x"},
		{"429限流-熔断器算失败-模型级", true, 429, false, "gpt-x"},
		{"401凭证类-上游级连坐", true, 401, false, ""},
		{"403凭证类-上游级连坐", true, 403, false, ""},
		{"500故障-模型级", true, 500, false, "gpt-x"},
		{"网络错误-模型级失败", false, 0, false, "gpt-x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fb := &fakeBreaker{}
			p := &Prober{breaker: fb}
			p.observe(m, c.hasResp, c.code, 100)
			if !fb.called {
				t.Fatal("应调用 ObserveProbe")
			}
			if fb.gotID != 7 {
				t.Fatalf("上游 ID 应透传 7，实际 %d", fb.gotID)
			}
			if fb.gotOK != c.wantOK {
				t.Fatalf("ok 应为 %v，实际 %v", c.wantOK, fb.gotOK)
			}
			if fb.gotModel != c.wantModel {
				t.Fatalf("scope 应为 %q，实际 %q", c.wantModel, fb.gotModel)
			}
		})
	}

	// breaker 为 nil 时 observe 不 panic（探测器可无熔断器纯看板模式）
	(&Prober{}).observe(m, true, 200, 10)
}
