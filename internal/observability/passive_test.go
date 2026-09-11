package observability

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mirainya/muxapi/internal/forward"
)

func TestPassiveSeparatesRequestsAndAttempts(t *testing.T) {
	p := New()
	p.SetAuditDropsProvider(func() uint64 { return 3 })
	p.Begin()
	p.Finish(forward.Result{
		Status: 200, Outcome: forward.OutcomeSuccess, ResponseBytes: 120,
		InputTokens: 10, OutputTokens: 5, CachedTokens: 2, CacheCreationTokens: 1,
		TTFTMs: 50,
		Attempts: []forward.AttemptResult{
			{Status: 503, Outcome: forward.OutcomeFailed},
			{Status: 200, Outcome: forward.OutcomeSuccess},
		},
	}, 250)

	s := p.Snapshot()
	if s.Requests != 1 || s.Attempts != 2 || s.Retried != 1 {
		t.Fatalf("request/attempt counters = %+v", s)
	}
	if s.Outcomes[forward.OutcomeSuccess] != 1 || s.Outcomes[forward.OutcomeFailed] != 0 {
		t.Fatalf("request outcomes = %+v", s.Outcomes)
	}
	if s.ResponseBytes != 120 || s.InputTokens != 10 || s.OutputTokens != 5 || s.CachedTokens != 2 || s.CacheCreated != 1 {
		t.Fatalf("request totals = %+v", s)
	}
	if s.DurationCount != 1 || s.DurationSumMs != 250 || s.TTFTCount != 1 || s.TTFTSumMs != 50 {
		t.Fatalf("latency totals = %+v", s)
	}
	if s.AuditDrops != 3 || s.InFlight != 0 {
		t.Fatalf("local state = %+v", s)
	}
}

func TestPassiveOutcomeClassification(t *testing.T) {
	cases := []struct {
		name          string
		request       string
		attempt       string
		status        int
		wantAttempts  uint64
		wantStatusCls string
	}{
		{name: "failed", request: forward.OutcomeFailed, attempt: forward.OutcomeFailed, status: 502, wantAttempts: 1, wantStatusCls: "5xx"},
		{name: "partial", request: forward.OutcomePartial, attempt: forward.OutcomePartial, status: 200, wantAttempts: 1, wantStatusCls: "2xx"},
		{name: "canceled", request: forward.OutcomeCanceled, attempt: forward.OutcomeCanceled, status: forward.StatusClientClosedRequest, wantAttempts: 1, wantStatusCls: "4xx"},
		{name: "client error", request: forward.OutcomeClientError, attempt: forward.OutcomeClientError, status: 400, wantAttempts: 1, wantStatusCls: "4xx"},
		{name: "unsupported", request: forward.OutcomeUnsupported, attempt: forward.OutcomeUnsupported, status: 404, wantAttempts: 1, wantStatusCls: "4xx"},
		{name: "unavailable", request: forward.OutcomeUnavailable, status: 0, wantAttempts: 0, wantStatusCls: ""},
		{name: "unknown", request: "unexpected_outcome", attempt: "unexpected_attempt", status: 0, wantAttempts: 0, wantStatusCls: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			p.Begin()
			result := forward.Result{Status: tc.status, Outcome: tc.request}
			if tc.wantAttempts > 0 {
				result.Attempts = []forward.AttemptResult{{Status: tc.status, Outcome: tc.attempt}}
			}
			p.Finish(result, 25)

			snapshot := p.Snapshot()
			wantOutcome := tc.request
			if wantOutcome != forward.OutcomeSuccess && wantOutcome != forward.OutcomeFailed &&
				wantOutcome != forward.OutcomeCanceled && wantOutcome != forward.OutcomePartial &&
				wantOutcome != forward.OutcomeClientError && wantOutcome != forward.OutcomeUnsupported &&
				wantOutcome != forward.OutcomeUnavailable {
				wantOutcome = "unknown"
			}
			if snapshot.Requests != 1 || snapshot.Outcomes[wantOutcome] != 1 {
				t.Fatalf("request outcome = %+v, want one %q: %+v", snapshot.Outcomes, wantOutcome, snapshot)
			}
			if snapshot.Attempts != tc.wantAttempts {
				t.Fatalf("attempt count = %d, want %d", snapshot.Attempts, tc.wantAttempts)
			}

			body := p.prometheus()
			attemptOutcome := tc.attempt
			if attemptOutcome != forward.OutcomeFailed && attemptOutcome != forward.OutcomePartial &&
				attemptOutcome != forward.OutcomeCanceled && attemptOutcome != forward.OutcomeClientError &&
				attemptOutcome != forward.OutcomeSuccess && attemptOutcome != forward.OutcomeUnsupported &&
				attemptOutcome != forward.OutcomeUnavailable {
				attemptOutcome = "unknown"
			}
			if got := metricValue(body, "muxapi_upstream_attempts_total", attemptOutcome); tc.wantAttempts == 0 {
				if got != 0 {
					t.Fatalf("attempt outcome %q = %d, want 0", attemptOutcome, got)
				}
			} else if got != 1 {
				t.Fatalf("attempt outcome %q = %d, want 1:\n%s", attemptOutcome, got, body)
			}
			if tc.wantStatusCls != "" {
				if snapshot.StatusClasses[tc.wantStatusCls] != 1 {
					t.Fatalf("status classes = %+v, want one %s", snapshot.StatusClasses, tc.wantStatusCls)
				}
			}
		})
	}
}

func TestPassiveExcludesGatewayCandidateFailuresFromUpstreamAttempts(t *testing.T) {
	p := New()
	p.Begin()
	p.Finish(forward.Result{
		Status: 200, Outcome: forward.OutcomeSuccess,
		Attempts: []forward.AttemptResult{
			{UpstreamID: 7, Status: 0, Outcome: forward.OutcomeUnsupported, ErrorSource: "gateway"},
			{UpstreamID: 8, Status: 200, Outcome: forward.OutcomeSuccess, NetworkAttempt: true},
		},
	}, 20)

	snapshot := p.Snapshot()
	if snapshot.Requests != 1 || snapshot.Attempts != 1 || snapshot.Retried != 0 {
		t.Fatalf("gateway-local candidate must not count as retry: %+v", snapshot)
	}
	if snapshot.Outcomes[forward.OutcomeSuccess] != 1 {
		t.Fatalf("request outcome = %+v", snapshot.Outcomes)
	}
}

func TestPassiveMetricsUseStableLabelsAndHistograms(t *testing.T) {
	p := New()
	p.Begin()
	p.Finish(forward.Result{
		Status: 499, Outcome: forward.OutcomeCanceled, TTFTMs: 1,
		Attempts: []forward.AttemptResult{{Status: 499, Outcome: forward.OutcomeCanceled}},
	}, 11)
	response := httptest.NewRecorder()
	p.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		"# TYPE muxapi_requests_total counter",
		`muxapi_requests_total{outcome="canceled"} 1`,
		`muxapi_upstream_attempts_total{outcome="canceled"} 1`,
		`muxapi_request_duration_seconds_bucket{le="0.05"} 1`,
		"muxapi_request_duration_seconds_count 1",
		"muxapi_request_audit_dropped_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "request_id") || strings.Contains(body, "session") || strings.Contains(body, "error_text") {
		t.Fatalf("metrics contain high-cardinality/request data:\n%s", body)
	}
}

func TestPassiveMetricsAcceptHeadAndRejectWrites(t *testing.T) {
	p := New()
	head := httptest.NewRecorder()
	p.ServeHTTP(head, httptest.NewRequest("HEAD", "/metrics", nil))
	if head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("HEAD response = status %d body %q, want 200/empty", head.Code, head.Body.String())
	}
	post := httptest.NewRecorder()
	p.ServeHTTP(post, httptest.NewRequest("POST", "/metrics", nil))
	if post.Code != 405 {
		t.Fatalf("POST response status = %d, want 405", post.Code)
	}
}

func TestPassiveFinishNeverMakesInflightNegative(t *testing.T) {
	p := New()
	p.Finish(forward.Result{Status: 200, Outcome: forward.OutcomeSuccess}, 0)
	snapshot := p.Snapshot()
	if snapshot.InFlight != 0 {
		t.Fatalf("in-flight = %d, want 0", snapshot.InFlight)
	}
	if snapshot.DurationCount != 1 || snapshot.DurationSumMs != 0 {
		t.Fatalf("zero duration sample = count %d sum %f, want 1/0", snapshot.DurationCount, snapshot.DurationSumMs)
	}
	if snapshot.TTFTCount != 0 {
		t.Fatalf("missing TTFT should not create a sample: %d", snapshot.TTFTCount)
	}
}

func metricValue(body, metric, outcome string) uint64 {
	prefix := metric + `{outcome="` + outcome + `"} `
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 10, 64)
		if err == nil {
			return value
		}
	}
	return 0
}
