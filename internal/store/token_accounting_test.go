package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/tokenusage"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestAccountingSnapshotSharedAcrossViewsAndAttempts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "accounting.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u := &upstream.Upstream{Name: "changing-protocol", BaseURL: "http://localhost", APIKey: "test", Protocol: "responses", Enabled: true}
	if err := s.Create(u); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e := tokenusage.Estimate{Tokens: 1000, Model: "gpt-5", Protocol: "responses", Source: "local_tiktoken", Covered: true}
	r := tokenusage.Parse(map[string]any{"input_tokens": int64(1000), "cached_tokens": int64(900)}, "responses")
	r.Complete = true
	m := tokenusage.New(e, r)
	firstEstimate := e
	firstEstimate.Tokens = 2000
	first := tokenusage.New(firstEstimate, r)
	record := RequestRecord{RequestID: "shared-measurement", FinalUpstreamID: u.ID, InputTokens: 100, InputTokensNormalized: true, CachedTokens: 900, TokenAccounting: m, CreatedAt: now, CompletedAt: now,
		Attempts: []RequestAttemptRecord{
			{AttemptNo: 1, UpstreamID: u.ID, Protocol: "responses", TokenAccounting: first, CreatedAt: now, CompletedAt: now},
			{AttemptNo: 2, UpstreamID: u.ID, Protocol: "responses", InputTokens: 100, InputTokensNormalized: true, CachedTokens: 900, TokenAccounting: m, CreatedAt: now, CompletedAt: now},
		},
	}
	if err := s.writeRequest(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRouteDecision(RouteDecisionRecord{RequestID: record.RequestID, Model: "client-alias", Protocol: "claude", EstimatedInputTokens: 9999, SelectedUpstreamID: u.ID}); err != nil {
		t.Fatal(err)
	}
	input, cached, normalized := int64(100), int64(900), true
	if err := s.CompleteRouteDecision(record.RequestID, RouteDecisionOutcome{ActualInputTokens: &input, ActualInputTokensNormalized: &normalized, ActualCachedTokens: &cached, ActualUpstreamID: u.ID, TokenAccounting: m}); err != nil {
		t.Fatal(err)
	}
	// An unrelated later reconciliation must not erase the original snapshot.
	if err := s.CompleteRouteDecision(record.RequestID, RouteDecisionOutcome{Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	verify := func() {
		t.Helper()
		page, err := s.ListRequestsPage(RequestFilter{})
		if err != nil || len(page.Entries) != 1 {
			t.Fatalf("page: %+v %v", page, err)
		}
		detail, err := s.GetRequest(page.Entries[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		route, err := s.GetRouteDecisionByRequestID(record.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []*tokenusage.Measurement{page.Entries[0].TokenAccounting, detail.TokenAccounting, detail.Attempts[1].TokenAccounting, route.TokenAccounting} {
			if tokenusage.Encode(got) != tokenusage.Encode(m) || got.Ratio == nil || *got.Ratio != 1 {
				t.Fatalf("views disagree: %+v", got)
			}
		}
		if detail.Attempts[0].TokenAccounting.Ratio == nil || *detail.Attempts[0].TokenAccounting.Ratio != .5 {
			t.Fatal("attempt baselines were mixed")
		}
		if detail.CacheInputTokens != 1000 || detail.CacheRate != .9 || route.ActualPromptTokens == nil || *route.ActualPromptTokens != 1000 {
			t.Fatal("input total mismatch")
		}
	}
	verify()
	u.Protocol = "claude"
	if err := s.Update(u); err != nil {
		t.Fatal(err)
	}
	verify()
	if err := s.Delete(u.ID); err != nil {
		t.Fatal(err)
	}
	verify()
}

func TestLegacyUsageUsesAttemptProtocolOrRemainsUnknown(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "legacy-accounting.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, tc := range []struct{ id, protocol string }{{"known", "responses"}, {"unknown", ""}} {
		if err := s.writeRequest(RequestRecord{RequestID: tc.id, InputTokens: 1000, CachedTokens: 900, CreatedAt: now, CompletedAt: now, Attempts: []RequestAttemptRecord{{AttemptNo: 1, Protocol: tc.protocol, CreatedAt: now, CompletedAt: now}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveRouteDecision(RouteDecisionRecord{RequestID: tc.id, Protocol: "claude", EstimatedInputTokens: 1000}); err != nil {
			t.Fatal(err)
		}
		input, cached := int64(1000), int64(900)
		if err := s.CompleteRouteDecision(tc.id, RouteDecisionOutcome{ActualInputTokens: &input, ActualCachedTokens: &cached}); err != nil {
			t.Fatal(err)
		}
		page, err := s.ListRequestsPage(RequestFilter{Query: tc.id})
		if err != nil || len(page.Entries) != 1 {
			t.Fatalf("legacy page: %+v %v", page, err)
		}
		entry := page.Entries[0]
		route, err := s.GetRouteDecisionByRequestID(tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if entry.TokenInflation != 0 || entry.TokenAccounting.Ratio != nil || route.TokenAccounting.Ratio != nil {
			t.Fatal("legacy baseline invented")
		}
		if tc.protocol != "" {
			if entry.CacheInputTokens != 1000 || route.ActualPromptTokens == nil || *route.ActualPromptTokens != 1000 {
				t.Fatal("legacy cached input counted twice")
			}
		} else if entry.TokenAccounting.Report.InputTotal != nil || route.ActualPromptTokens != nil {
			t.Fatal("client protocol used as outgoing protocol")
		}
	}
}
