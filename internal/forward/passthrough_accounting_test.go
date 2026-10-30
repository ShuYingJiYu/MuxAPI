package forward

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

// A passthrough channel forwards the client payload unchanged, so the protocol
// spoken on the wire is the client's own format even though the channel is
// configured as "passthrough". The v0.2.15 token accounting keyed both the
// outbound estimate and the response usage parser on that literal channel
// setting, which tokenusage.Protocol collapses to empty. Every request on a
// passthrough channel then reported unknown_protocol with a zero-token
// estimate and no comparable ratio: on US production 189 of 223 accounted
// decisions in the 90 minutes after rollout were unknown_protocol against 0
// comparable, and OpenAI-shaped usage was misread as Claude (29 invalid_usage).
func TestPassthroughChannelMeasuresWireProtocol(t *testing.T) {
	upstreamBody := `{"id":"chat_1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":2944,"cached_tokens":2944,"completion_tokens":2227,"prompt_tokens":3146,"prompt_tokens_details":{"cached_tokens":2944},"total_tokens":5373}}`
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upSrv.Close()

	ups := []*upstream.Upstream{{ID: 1, BaseURL: upSrv.URL, APIKey: "k", Protocol: "passthrough", Priority: 1, Weight: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 2)

	// gpt-4o has a verified local tokenizer, so a measured payload and a valid
	// usage report must reach the comparable state with a real ratio.
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello world"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	result := fwd.Forward(httptest.NewRecorder(), req, body, 1, "")

	if result.Outcome != OutcomeSuccess {
		t.Fatalf("outcome=%q want success", result.Outcome)
	}
	m := result.TokenAccounting
	if m == nil {
		t.Fatal("passthrough attempt produced no token accounting")
	}
	if m.Estimate.Protocol != "openai" {
		t.Fatalf("estimate protocol=%q want openai: the estimate must describe the wire format, not the channel setting", m.Estimate.Protocol)
	}
	if m.Estimate.Reason == "unknown_protocol" {
		t.Fatal("passthrough request was rejected as an unknown protocol instead of being measured")
	}
	if m.Estimate.Tokens <= 0 {
		t.Fatalf("estimate tokens=%d want >0: the outgoing payload must be counted", m.Estimate.Tokens)
	}
	if m.Report.Protocol != "openai" || m.Report.Quality != "valid" {
		t.Fatalf("usage protocol=%q quality=%q: OpenAI-shaped usage must be parsed as OpenAI, not inferred as Claude", m.Report.Protocol, m.Report.Quality)
	}
	if m.Report.InputTotal == nil || *m.Report.InputTotal != 3146 {
		t.Fatalf("input total=%v want 3146", m.Report.InputTotal)
	}
	if m.Status != "comparable" || m.Ratio == nil {
		t.Fatalf("status=%q ratio=%v: a measured payload plus valid usage must be comparable", m.Status, m.Ratio)
	}
}

// An unverified tokenizer keeps the honest unverified_tokenizer status, but the
// estimate must still be counted rather than dropped as unknown_protocol.
func TestPassthroughUnverifiedTokenizerStillMeasures(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat_1","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":2944,"cached_tokens":2944,"completion_tokens":2227,"prompt_tokens":3146,"prompt_tokens_details":{"cached_tokens":2944},"total_tokens":5373}}`)
	}))
	defer upSrv.Close()

	ups := []*upstream.Upstream{{ID: 1, BaseURL: upSrv.URL, APIKey: "k", Protocol: "passthrough", Priority: 1, Weight: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 2)

	body := []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hello world"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	result := fwd.Forward(httptest.NewRecorder(), req, body, 1, "")

	m := result.TokenAccounting
	if m == nil {
		t.Fatal("passthrough attempt produced no token accounting")
	}
	if m.Estimate.Tokens <= 0 || m.Estimate.Reason == "unknown_protocol" {
		t.Fatalf("estimate tokens=%d reason=%q: an unverified tokenizer must still count the payload", m.Estimate.Tokens, m.Estimate.Reason)
	}
	if m.Status != "unverified_tokenizer" {
		t.Fatalf("status=%q want unverified_tokenizer", m.Status)
	}
	if m.Report.Protocol != "openai" || m.Report.Quality != "valid" {
		t.Fatalf("usage protocol=%q quality=%q want openai/valid", m.Report.Protocol, m.Report.Quality)
	}
}
