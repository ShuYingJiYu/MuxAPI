package tokenusage

import (
	"encoding/json"
	"strings"
	"testing"
)

func parse(t *testing.T, body, protocol string) Report {
	t.Helper()
	var value map[string]any
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return Parse(value, protocol)
}

func TestUsageBuckets(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, body, quality       string
		total, input, cached, write, output int64
	}{
		{"responses", "codex", `{"input_tokens":1000,"input_tokens_details":{"cached_tokens":900},"output_tokens":10}`, "valid", 1000, 100, 900, 0, 10},
		{"nested_write", "openai", `{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":600,"cache_creation_tokens":300}}`, "valid", 1000, 100, 600, 300, 0},
		{"claude", "claude", `{"input_tokens":100,"cache_read_input_tokens":600,"cache_creation_input_tokens":300,"output_tokens":10}`, "valid", 1000, 100, 600, 300, 10},
		{"gemini_thoughts", "gemini", `{"promptTokenCount":1000,"cachedContentTokenCount":900,"candidatesTokenCount":10,"thoughtsTokenCount":20,"totalTokenCount":1030}`, "valid", 1000, 100, 900, 0, 30},
		{"explicit_zero", "responses", `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, "valid", 0, 0, 0, 0, 0},
		{"ambiguous_write", "openai", `{"input_tokens":1000,"cached_tokens":600,"cache_creation_input_tokens":300}`, "unclassified", 1000, 400, 600, 300, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parse(t, tc.body, tc.protocol)
			if r.Quality != tc.quality || r.InputTotal == nil || *r.InputTotal != tc.total || r.UncachedInput != tc.input || r.CacheRead != tc.cached || r.CacheWrite != tc.write || r.Output != tc.output {
				t.Fatalf("unexpected report: %+v", r)
			}
		})
	}
}

func TestInvalidUsageCannotProduceRatio(t *testing.T) {
	for _, body := range []string{
		`{"input_tokens":-1}`, `{"input_tokens":1.5}`, `{"input_tokens":9223372036854775808}`,
		`{"input_tokens":100,"prompt_tokens":101}`, `{"input_tokens":10,"cached_tokens":11}`,
		`{"input_tokens":10,"output_tokens":2,"total_tokens":99}`, `{"input_tokens":null}`,
		`{"input_tokens":100,"cached_tokens":80,"input_tokens_details":{"cached_tokens":90}}`,
		`{"input_tokens":{"secret":"must not persist"}}`,
	} {
		r := parse(t, body, "responses")
		r.Complete = true
		m := New(Estimate{Tokens: 100, Covered: true}, r)
		if m.Status != "invalid_usage" || m.Ratio != nil {
			t.Fatalf("invalid report accepted: %s: %+v", body, m)
		}
		if strings.Contains(Encode(m), "must not persist") {
			t.Fatal("arbitrary payload persisted")
		}
	}
	r := parse(t, `{"input_tokens":-1}`, "responses")
	if r.Raw["input_tokens"] != "-1" {
		t.Fatal("original invalid numeric value lost")
	}
	r = parse(t, `{"input_tokens":9223372036854775807,"cache_read_input_tokens":1}`, "claude")
	if r.Quality != "inconsistent" || r.InputTotal != nil {
		t.Fatalf("overflow accepted: %+v", r)
	}
}

func TestMeasurementAvailabilityAndRoundTrip(t *testing.T) {
	e := Estimate{Tokens: 1000, Model: "gpt-5", Protocol: "responses", Source: "local_tiktoken", Covered: true}
	r := parse(t, `{"input_tokens":1000,"input_tokens_details":{"cached_tokens":900}}`, "responses")
	r.Complete = true
	m := New(e, r)
	if m.Ratio == nil || *m.Ratio != 1 {
		t.Fatalf("cache counted twice: %+v", m)
	}
	m.Ratio = new(float64) // Persistence cannot override the underlying measurement.
	if got := Decode(Encode(m)); got == nil || got.Ratio == nil || *got.Ratio != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	for _, tc := range []struct {
		name, status string
		change       func(*Estimate, *Report)
	}{
		{"missing", "missing_usage", func(e *Estimate, r *Report) { *r = Report{} }},
		{"partial", "incomplete_usage", func(e *Estimate, r *Report) { r.Complete = false }},
		{"opaque", "server_context", func(e *Estimate, r *Report) { e.Covered = false; e.Reason = "server_context" }},
		{"unknown", "unclassified_usage", func(e *Estimate, r *Report) { r.Quality = "unclassified" }},
		{"empty", "empty_estimate", func(e *Estimate, r *Report) { e.Tokens = 0 }},
		{"model", "response_model_mismatch", func(e *Estimate, r *Report) { r.ResponseModel = "gpt-4o" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := e, r
			tc.change(&a, &b)
			got := New(a, b)
			if got.Status != tc.status || got.Ratio != nil {
				t.Fatalf("unexpected comparison: %+v", got)
			}
		})
	}
	r.ResponseModel = "gpt-5-2026-10-30"
	if New(e, r).Ratio == nil {
		t.Fatal("dated model rejected")
	}
	r.ResponseModel = "gpt-5-2026-99-99"
	if New(e, r).Ratio != nil {
		t.Fatal("invalid model date accepted")
	}
	r = parse(t, `{"input_tokens":0}`, "responses")
	r.Complete = true
	if got := New(e, r); got.Ratio == nil || *got.Ratio != 0 {
		t.Fatal("zero report confused with missing usage")
	}
}

func TestLegacyHasNoIndependentRatio(t *testing.T) {
	for _, tc := range []struct {
		normalized bool
		protocol   string
		want       int64
	}{
		{false, "responses", 1000}, {false, "claude", 1900}, {true, "responses", 1900},
	} {
		m := Legacy(1000, 900, 0, tc.normalized, tc.protocol)
		if m.Status != "legacy" || m.Ratio != nil || m.Report.InputTotal == nil || *m.Report.InputTotal != tc.want {
			t.Fatalf("legacy mismatch: %+v", m)
		}
	}
	if Legacy(1000, 900, 0, false, "").Report.InputTotal != nil {
		t.Fatal("unknown historical protocol was guessed")
	}
}
