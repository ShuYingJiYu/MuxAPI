package forward

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/routing"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

type accountingModelMapper struct{}

func (accountingModelMapper) Resolve(id int64, model string) string {
	if id == 1 {
		return "claude-sonnet-4"
	}
	return "gpt-4o"
}
func (accountingModelMapper) RecordSuccess(int64, string) {}
func (accountingModelMapper) RecordFailure(int64, string) {}

func TestAccountingUsesFinalMappedPayloadPerAttempt(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary failure", http.StatusBadGateway)
	}))
	defer bad.Close()
	var sent []byte
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		sent, err = io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat_1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":900},"completion_tokens":2}}`)
	}))
	defer good.Close()
	ups := []*upstream.Upstream{
		{ID: 1, Protocol: "claude", BaseURL: bad.URL, APIKey: "test", Priority: 1, Weight: 1},
		{ID: 2, Protocol: "openai", BaseURL: good.URL, APIKey: "test", Priority: 2, Weight: 1},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, 2)
	fwd.SetModelMapper(accountingModelMapper{})
	body := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"hello"}],"tools":[{"type":"function","name":"lookup","description":"Look up a directory entry","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	result := fwd.Forward(httptest.NewRecorder(), req, body, 1, "")
	if result.Outcome != OutcomeSuccess || len(result.Attempts) != 2 {
		t.Fatalf("unexpected attempts: %+v", result)
	}
	want := routing.EstimateOutboundInput(sent, "gpt-4o", "openai")
	m := result.TokenAccounting
	if m == nil || m.Estimate != want || !m.Estimate.Covered || m.Ratio == nil || m.Report.InputTotal == nil || *m.Report.InputTotal != 1000 {
		t.Fatalf("actual outgoing payload was not measured: %+v want=%+v sent=%s", m, want, sent)
	}
	if result.Attempts[0].TokenAccounting.Estimate.Model != "claude-sonnet-4" || result.Attempts[0].TokenAccounting.Ratio != nil {
		t.Fatal("failed attempt mixed with successful usage")
	}
	if result.Attempts[1].TokenAccounting != m {
		t.Fatal("final request must retain final attempt measurement")
	}
}
