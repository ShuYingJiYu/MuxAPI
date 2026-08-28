package forward

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestStripContextSuffix(t *testing.T) {
	cases := [...]struct{ in, want string }{
		{"stealth/ox-alpha[1M]", "stealth/ox-alpha"},
		{"stealth/ox-alpha[1m]", "stealth/ox-alpha"},
		{"gpt-5[200k]", "gpt-5"},
		{"a[b]c[1M]", "a[b]c"},
		{"stealth/ox-alpha", "stealth/ox-alpha"},
		{"stealth/ox-alpha[1X]", "stealth/ox-alpha[1X]"},
		{"stealth/ox-alpha[M]", "stealth/ox-alpha[M]"},
		{"[1M]", "[1M]"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripContextSuffix(tc.in); got != tc.want {
			t.Errorf("stripContextSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseModelAliases(t *testing.T) {
	if got := ParseModelAliases(""); got != nil {
		t.Fatalf("empty config should give nil map, got %v", got)
	}
	got := ParseModelAliases("claude-opus-4-7=stealth/ox-alpha, bad-pair, claude-opus-4-7[1M]=other")
	if len(got) != 1 {
		t.Fatalf("want 1 alias after dedupe, got %v", got)
	}
	// 后写的覆盖先写的；from 的上下文后缀已剥离，两条落到同一个 key。
	if got["claude-opus-4-7"] != "other" {
		t.Fatalf("later duplicate should win, got %v", got)
	}
}

func TestCanonicalizeModel(t *testing.T) {
	fwd := &Forwarder{}
	if got := fwd.canonicalizeModel("stealth/ox-alpha[1M]"); got != "stealth/ox-alpha" {
		t.Fatalf("suffix not stripped: %q", got)
	}
	fwd = &Forwarder{modelAliases: map[string]string{"claude-opus-4-7": "stealth/ox-alpha"}}
	cases := [...]struct{ in, want string }{
		{"claude-opus-4-7", "stealth/ox-alpha"},
		{"claude-opus-4-7[1M]", "stealth/ox-alpha"},
		{"unknown-model", "unknown-model"},
	}
	for _, tc := range cases {
		if got := fwd.canonicalizeModel(tc.in); got != tc.want {
			t.Errorf("canonicalizeModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 透传路径（openai→openai）也必须把归一化后的模型名送到上游。
func TestForwardCanonicalizesClientModelName(t *testing.T) {
	var gotModel string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotModel = payload.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstreamServer.Close()

	ups := []*upstream.Upstream{{ID: 1, BaseURL: upstreamServer.URL, APIKey: "key", Protocol: "openai", Priority: 1, Weight: 1}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 1)
	fwd.SetModelAliases(map[string]string{"claude-opus-4-7": "stealth/ox-alpha"})

	body := []byte(`{"model":"claude-opus-4-7[1M]","messages":[{"role":"user","content":"hi"}]}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	result := fwd.Forward(recorder, request, body, 1, "")
	if result.Status != http.StatusOK {
		t.Fatalf("forward failed: %+v", result)
	}
	if gotModel != "stealth/ox-alpha" {
		t.Fatalf("upstream saw model %q, want stealth/ox-alpha", gotModel)
	}
}
