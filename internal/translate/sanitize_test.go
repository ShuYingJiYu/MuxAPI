package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// deferredClaudeRequest 复刻 Claude Code 2.1.24x 开启 ToolSearch 后的真实形状：
// 大部分工具被省略，只剩核心工具 + ToolSearch + 占位工具，历史里引用了
// 被省略的 MCP 工具。
const deferredClaudeRequest = `{"model":"stealth/ox-alpha","stream":true,
  "system":[{"type":"text","text":"deferred tools: mcp__ghidra__connect_instance"}],
  "tools":[
    {"name":"Bash","description":"run commands","input_schema":{"type":"object"}},
    {"name":"ToolSearch","description":"search deferred tools","input_schema":{"type":"object"}},
    {"name":"DeferredToolPlaceholder","description":"Reserved placeholder that keeps deferred tool loading active; never call this tool.","input_schema":{"type":"object"},"defer_loading":true}
  ],
  "messages":[
    {"role":"user","content":[{"type":"text","text":"list instances"}]},
    {"role":"assistant","content":[
      {"type":"tool_use","id":"toolu_1","name":"ToolSearch","input":{"query":"ghidra"}},
      {"type":"tool_use","id":"toolu_2","name":"mcp__ghidra__connect_instance","input":{}}
    ]},
    {"role":"user","content":[
      {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"mcp__ghidra__connect_instance"}]},
      {"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"connected"}]}
    ]}
  ]}`

type toolProbe struct {
	Name     string         `json:"name"`
	Function *struct {
		Name string `json:"name"`
	} `json:"function"`
}

func upstreamToolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var parsed struct {
		Tools []toolProbe `json:"tools"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, body)
	}
	names := make([]string, 0, len(parsed.Tools))
	for _, tool := range parsed.Tools {
		if tool.Function != nil {
			names = append(names, tool.Function.Name)
		} else {
			names = append(names, tool.Name)
		}
	}
	return names
}

// 翻译到 openai 渠道时：占位工具必须消失，defer_loading 不得泄漏，
// 历史里引用过但未定义的 MCP 工具要补上 stub 定义。
func TestSanitizeOpenAIInjectsMissingDeferredTools(t *testing.T) {
	exchange, err := NewExchange(Claude, OpenAI, "stealth/ox-alpha", true, []byte(deferredClaudeRequest))
	if err != nil {
		t.Fatal(err)
	}
	body := string(exchange.UpstreamRequest)
	if strings.Contains(body, deferredToolPlaceholder) {
		t.Fatalf("placeholder leaked to upstream: %s", body)
	}
	if strings.Contains(body, "defer_loading") {
		t.Fatalf("defer_loading leaked to upstream: %s", body)
	}
	names := upstreamToolNames(t, exchange.UpstreamRequest)
	want := map[string]bool{
		"Bash":                            false,
		"ToolSearch":                      false,
		"mcp__ghidra__connect_instance":   false,
	}
	for _, name := range names {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("upstream tools missing %q: %v\n%s", name, names, body)
		}
	}
	if !strings.Contains(body, stubToolDescription) {
		t.Fatalf("injected stub lacks gateway description: %s", body)
	}
}

// 透传到 claude 渠道时：剥 defer_loading、删占位、注入 anthropic 形状的 stub。
func TestSanitizePassthroughClaudeStripsDeferral(t *testing.T) {
	original := []byte(deferredClaudeRequest)
	exchange, err := NewExchange(Claude, Claude, "stealth/ox-alpha", true, original)
	if err != nil {
		t.Fatal(err)
	}
	if exchange.Translated() {
		t.Fatal("claude -> claude must stay passthrough")
	}
	body := string(exchange.UpstreamRequest)
	if strings.Contains(body, deferredToolPlaceholder) {
		t.Fatalf("placeholder leaked to upstream: %s", body)
	}
	if strings.Contains(body, "defer_loading") {
		t.Fatalf("defer_loading leaked to upstream: %s", body)
	}
	names := upstreamToolNames(t, exchange.UpstreamRequest)
	found := false
	for _, name := range names {
		if name == "mcp__ghidra__connect_instance" {
			found = true
		}
	}
	if !found {
		t.Fatalf("passthrough tools missing injected stub: %v\n%s", names, body)
	}
	var probe struct {
		Model  string `json:"model"`
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(exchange.UpstreamRequest, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Model != "stealth/ox-alpha" || len(probe.System) == 0 {
		t.Fatalf("non-tool fields damaged: model=%q system=%s", probe.Model, probe.System)
	}
}

// 普通请求必须原样通过，不得多出任何 stub。
func TestSanitizeNoopForPlainRequest(t *testing.T) {
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`)

	passthrough, err := NewExchange(Claude, Claude, "m", true, plain)
	if err != nil {
		t.Fatal(err)
	}
	if string(passthrough.UpstreamRequest) != string(plain) {
		t.Fatalf("plain passthrough body was altered:\n%s", passthrough.UpstreamRequest)
	}

	translated, err := NewExchange(Claude, OpenAI, "m", true, plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(translated.UpstreamRequest), stubToolDescription) {
		t.Fatalf("stub injected into a plain request: %s", translated.UpstreamRequest)
	}
}

// 引用已定义的工具时不得重复注入。
func TestSanitizeDoesNotDuplicateDefinedTools(t *testing.T) {
	body := `{"model":"m","stream":true,"tools":[
      {"name":"Bash","description":"run","input_schema":{"type":"object"}}
    ],
    "messages":[
      {"role":"user","content":[{"type":"text","text":"run ls"}]},
      {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},
      {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]}]}
    ]}`
	exchange, err := NewExchange(Claude, OpenAI, "m", true, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, name := range upstreamToolNames(t, exchange.UpstreamRequest) {
		if name == "Bash" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Bash defined %d times, want 1: %s", count, exchange.UpstreamRequest)
	}
	if strings.Contains(string(exchange.UpstreamRequest), stubToolDescription) {
		t.Fatalf("unexpected stub for a defined tool: %s", exchange.UpstreamRequest)
	}
}
