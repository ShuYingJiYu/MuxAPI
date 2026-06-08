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
	cases := []struct {
		name      string
		channel   bool // 该上游是否开启渠道级探测
		hasResp   bool
		code      int
		wantOK    bool
		wantModel string
	}{
		// 渠道级关：成功只复活该模型
		{"关-2xx成功-模型级", false, true, 200, true, "gpt-x"},
		{"关-429限流-熔断器算失败-模型级", false, true, 429, false, "gpt-x"},
		{"关-401凭证类-上游级连坐", false, true, 401, false, ""},
		{"关-403凭证类-上游级连坐", false, true, 403, false, ""},
		{"关-500故障-模型级", false, true, 500, false, "gpt-x"},
		{"关-网络错误-模型级失败", false, false, 0, false, "gpt-x"},
		// 渠道级开：成功复活整渠道(scope="")；失败口径不变
		{"开-2xx成功-渠道级复活", true, true, 200, true, ""},
		{"开-401凭证类-上游级连坐", true, true, 401, false, ""},
		{"开-500故障-仍模型级", true, true, 500, false, "gpt-x"},
		{"开-网络错误-仍模型级失败", true, false, 0, false, "gpt-x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &store.Monitor{UpstreamID: 7, Model: "gpt-x", ChannelProbe: c.channel}
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
	(&Prober{}).observe(&store.Monitor{UpstreamID: 7, Model: "gpt-x"}, true, 200, 10)
}
