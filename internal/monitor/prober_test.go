package monitor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/store"
)

// probeCall 记一次喂给熔断器的调用（探测成功会驱动两次：模型级 + 上游级）。
type probeCall struct {
	id    int64
	model string
	ok    bool
}

// fakeBreaker 捕获 observe 喂给熔断器的【全部】调用，按顺序记录。
type fakeBreaker struct {
	calls []probeCall
}

func (f *fakeBreaker) ObserveProbe(id int64, model string, ok bool, latencyMs int64) {
	f.calls = append(f.calls, probeCall{id, model, ok})
}

// hasUpstreamRevive 是否含一次「复活上游级键(scope="", ok=true)」的调用。
func (f *fakeBreaker) hasUpstreamRevive(id int64) bool {
	for _, c := range f.calls {
		if c.id == id && c.model == "" && c.ok {
			return true
		}
	}
	return false
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
//   - 429 → 看板算降级，但熔断器算【失败】；渠道级探测开时熔整渠道，否则只熔模型
//   - 401 → 凭证类，熔断器记上游级(scope="")，所有模型连坐
//   - 网络错误 → 渠道级探测开时熔整渠道，否则只熔模型

func TestObserveBreakerScope(t *testing.T) {
	cases := []struct {
		name      string
		channel   bool
		hasResp   bool
		code      int
		wantOK    bool
		wantModel string
	}{
		{"off-2xx-model-success", false, true, 200, true, "gpt-x"},
		{"off-429-model-fail", false, true, 429, false, "gpt-x"},
		{"off-401-upstream-fail", false, true, 401, false, ""},
		{"off-403-upstream-fail", false, true, 403, false, ""},
		{"off-500-model-fail", false, true, 500, false, "gpt-x"},
		{"off-network-model-fail", false, false, 0, false, "gpt-x"},
		{"on-2xx-channel-success", true, true, 200, true, ""},
		{"on-401-upstream-fail", true, true, 401, false, ""},
		{"on-500-channel-fail", true, true, 500, false, ""},
		{"on-network-channel-fail", true, false, 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &store.Monitor{UpstreamID: 7, Model: "gpt-x", ChannelProbe: c.channel}
			fb := &fakeBreaker{}
			p := &Prober{breaker: fb}
			p.observe(m, c.hasResp, c.code, 100)
			if len(fb.calls) != 1 {
				t.Fatalf("expected exactly one breaker call, got %+v", fb.calls)
			}
			got := fb.calls[0]
			if got.id != 7 || got.ok != c.wantOK || got.model != c.wantModel {
				t.Fatalf("unexpected call: got=%+v want id=7 ok=%v model=%q", got, c.wantOK, c.wantModel)
			}
		})
	}

	(&Prober{}).observe(&store.Monitor{UpstreamID: 7, Model: "gpt-x"}, true, 200, 10)
}

func TestChannelProbeRevivesUpstreamKeyOnSuccess(t *testing.T) {
	const id = int64(88)
	mgr := health.New(3, time.Minute)
	p := &Prober{breaker: mgr}
	m := &store.Monitor{UpstreamID: id, Model: "gpt-x", ChannelProbe: true}

	p.observe(m, true, 401, 0)
	if mgr.IsAvailable(id, "gpt-x") {
		t.Fatal("401 probe should block the upstream")
	}
	if got := mgr.EffectiveState(id); got != "OPEN" {
		t.Fatalf("state should be OPEN after upstream failure, got %q", got)
	}

	p.observe(m, true, 200, 50)
	if !mgr.IsAvailable(id, "gpt-x") {
		t.Fatal("channel probe success should revive the upstream")
	}
	if got := mgr.EffectiveState(id); got != "CLOSED" {
		t.Fatalf("state should be CLOSED after channel probe success, got %q", got)
	}
}

func TestEffIntervalLowerBound(t *testing.T) {
	const globalIV = 5 * time.Minute
	p := NewProber(nil, nil, nil, func() time.Duration { return globalIV }, nil)

	cases := []struct {
		name string
		sec  int
		want time.Duration
	}{
		{"过小抬到下限", 1, minIntervalSec * time.Second},
		{"恰好低于下限", minIntervalSec - 1, minIntervalSec * time.Second},
		{"等于下限保持", minIntervalSec, minIntervalSec * time.Second},
		{"高于下限照用", 120, 120 * time.Second},
		{"0 沿用全局默认", 0, globalIV},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.effInterval(&store.Monitor{IntervalSec: c.sec})
			if got != c.want {
				t.Fatalf("interval_sec=%d 应得 %v，实际 %v", c.sec, c.want, got)
			}
		})
	}
}
