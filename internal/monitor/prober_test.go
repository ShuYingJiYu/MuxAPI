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
			if len(fb.calls) == 0 {
				t.Fatal("应调用 ObserveProbe")
			}
			// 第一次调用＝主口径（模型级/渠道级/上游级失败）。
			first := fb.calls[0]
			if first.id != 7 {
				t.Fatalf("上游 ID 应透传 7，实际 %d", first.id)
			}
			if first.ok != c.wantOK {
				t.Fatalf("ok 应为 %v，实际 %v", c.wantOK, first.ok)
			}
			if first.model != c.wantModel {
				t.Fatalf("scope 应为 %q，实际 %q", c.wantModel, first.model)
			}
			// 成功时总应额外复活上游级键(凭证维度)；失败时不应有上游级复活。
			revived := fb.hasUpstreamRevive(7)
			if c.wantOK && !revived {
				t.Fatalf("探测成功应复活上游级键(scope=\"\",ok)，调用序列=%+v", fb.calls)
			}
			if !c.wantOK && revived {
				t.Fatalf("探测失败不应复活上游级键，调用序列=%+v", fb.calls)
			}
		})
	}

	// breaker 为 nil 时 observe 不 panic（探测器可无熔断器纯看板模式）
	(&Prober{}).observe(&store.Monitor{UpstreamID: 7, Model: "gpt-x"}, true, 200, 10)
}

// 端到端回归：ChannelProbe=false 时，凭证类(401)故障熔断上游级键后，
// 后续探测成功必须能把上游级键复活到 CLOSED——否则上游级键只进不出、
// 看板长期失真（修复前：探测成功只复活模型级键，上游级键卡 OPEN/HALF_OPEN）。
func TestProbeRevivesUpstreamKeyOnSuccess(t *testing.T) {
	const id = int64(88)
	mgr := health.New(3, time.Minute) // 业务阈值3；探测口径阈值1，401一次即上游级 OPEN
	p := &Prober{breaker: mgr}
	m := &store.Monitor{UpstreamID: id, Model: "gpt-x", ChannelProbe: false}

	// 1) 凭证类失败：探测 401 → 上游级键 OPEN，整上游不可用
	p.observe(m, true, 401, 0)
	if mgr.IsAvailable(id, "gpt-x") {
		t.Fatal("401 探测后整上游应不可用")
	}
	if got := mgr.EffectiveState(id); got != "OPEN" {
		t.Fatalf("凭证类熔断后对外状态应 OPEN，实际 %q", got)
	}

	// 2) 凭证修好：探测 200 → 应复活上游级键（缺陷修复点），整上游恢复可用
	p.observe(m, true, 200, 50)
	if !mgr.IsAvailable(id, "gpt-x") {
		t.Fatal("探测成功后上游级键应被复活、整上游恢复可用（缺陷：修复前会卡 OPEN）")
	}
	if got := mgr.EffectiveState(id); got != "CLOSED" {
		t.Fatalf("探测成功后对外状态应 CLOSED，实际 %q", got)
	}
}
// 防单项探测尚未返回就被反复重叠派发；0 仍表示沿用全局默认，不受下限影响。
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
