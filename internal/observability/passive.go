// Package observability contains low-overhead observations of real gateway
// traffic. It deliberately does not perform probes or feed the breaker.
package observability

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/mirainya/muxapi/internal/forward"
)

const (
	histogramBucketCount = 14
	// The last entry is the +Inf bucket. Values are seconds for exposition.
	requestDurationBuckets = histogramBucketCount - 1
)

var histogramBounds = [...]float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300,
}

var outcomeNames = [...]string{
	forward.OutcomeSuccess,
	forward.OutcomeFailed,
	forward.OutcomeCanceled,
	forward.OutcomePartial,
	forward.OutcomeClientError,
	forward.OutcomeUnsupported,
	forward.OutcomeUnavailable,
	"unknown",
}

type histogram struct {
	// Bucket counts are non-cumulative. Render converts them to the cumulative
	// representation required by the Prometheus text format.
	buckets  [histogramBucketCount]atomic.Uint64
	sumNanos atomic.Uint64
}

func (h *histogram) observeMillis(value int64) {
	if value < 0 {
		return
	}
	seconds := float64(value) / 1000
	index := requestDurationBuckets
	for i, bound := range histogramBounds {
		if seconds <= bound {
			index = i
			break
		}
	}
	h.buckets[index].Add(1)
	h.sumNanos.Add(uint64(value) * 1_000_000)
}

func (h *histogram) observePositiveMillis(value int64) {
	if value <= 0 {
		return
	}
	h.observeMillis(value)
}

func (h *histogram) snapshot() (buckets [histogramBucketCount]uint64, sumNanos uint64, count uint64) {
	for i := range h.buckets {
		buckets[i] = h.buckets[i].Load()
		count += buckets[i]
	}
	return buckets, h.sumNanos.Load(), count
}

type counters struct {
	requests       atomic.Uint64
	attempts       atomic.Uint64
	retried        atomic.Uint64
	bytes          atomic.Uint64
	input          atomic.Uint64
	output         atomic.Uint64
	cached         atomic.Uint64
	created        atomic.Uint64
	requestOutcome [len(outcomeNames)]atomic.Uint64
	attemptOutcome [len(outcomeNames)]atomic.Uint64
	requestStatus  [6]atomic.Uint64
}

// Snapshot is the JSON-safe view used by the admin endpoint. It intentionally
// contains only process-local cumulative data; durable history remains in the
// request audit tables.
type Snapshot struct {
	Requests      uint64            `json:"requests"`
	Attempts      uint64            `json:"attempts"`
	Retried       uint64            `json:"retried"`
	InFlight      int64             `json:"in_flight"`
	ResponseBytes uint64            `json:"response_bytes"`
	InputTokens   uint64            `json:"input_tokens"`
	OutputTokens  uint64            `json:"output_tokens"`
	CachedTokens  uint64            `json:"cached_tokens"`
	CacheCreated  uint64            `json:"cache_creation_tokens"`
	AuditDrops    uint64            `json:"audit_drops"`
	Outcomes      map[string]uint64 `json:"outcomes"`
	StatusClasses map[string]uint64 `json:"status_classes"`
	DurationCount uint64            `json:"duration_count"`
	DurationSumMs float64           `json:"duration_sum_ms"`
	TTFTCount     uint64            `json:"ttft_count"`
	TTFTSumMs     float64           `json:"ttft_sum_ms"`
}

// Passive records observations produced by actual client requests. New is the
// preferred constructor so future initialization can remain centralized.
type Passive struct {
	requests   counters
	duration   histogram
	ttft       histogram
	inFlight   atomic.Int64
	auditDrops func() uint64
}

// New creates a passive observer with no external side effects.
func New() *Passive { return &Passive{} }

// SetAuditDropsProvider supplies the cumulative request-audit loss count. The
// callback is evaluated only while rendering a snapshot, never on the request
// hot path.
func (p *Passive) SetAuditDropsProvider(provider func() uint64) {
	p.auditDrops = provider
}

// Begin marks a request as in flight. Call Finish exactly once for every Begin.
func (p *Passive) Begin() { p.inFlight.Add(1) }

// Finish records one completed client request and all of its upstream
// attempts. durationMs is measured at the HTTP boundary, so it includes the
// full failover chain rather than only the final attempt. It does not call
// health or routing code, so passive observation cannot duplicate breaker
// feedback.
func (p *Passive) Finish(result forward.Result, durationMs int64) {
	decrementNonNegative(&p.inFlight)
	p.requests.requests.Add(1)
	p.requests.requestOutcome[outcomeIndex(result.Outcome)].Add(1)
	p.requests.requestStatus[statusIndex(result.Status)].Add(1)
	p.requests.bytes.Add(nonNegative(result.ResponseBytes))
	p.requests.input.Add(nonNegative(result.InputTokens))
	p.requests.output.Add(nonNegative(result.OutputTokens))
	p.requests.cached.Add(nonNegative(result.CachedTokens))
	p.requests.created.Add(nonNegative(result.CacheCreationTokens))
	p.duration.observeMillis(durationMs)
	p.ttft.observePositiveMillis(result.TTFTMs)
	networkAttempts := 0
	for _, attempt := range result.Attempts {
		if !attempt.ReachedUpstream() {
			continue
		}
		networkAttempts++
		p.requests.attempts.Add(1)
		p.requests.attemptOutcome[outcomeIndex(attempt.Outcome)].Add(1)
	}
	if networkAttempts > 1 {
		p.requests.retried.Add(1)
	}
}

// Snapshot returns a consistent-enough point-in-time view for diagnostics.
// Individual fields may advance while the snapshot is being assembled.
func (p *Passive) Snapshot() Snapshot {
	outcomes := make(map[string]uint64, len(outcomeNames))
	for i, name := range outcomeNames {
		outcomes[name] = p.requests.requestOutcome[i].Load()
	}
	statusClasses := map[string]uint64{
		"none": p.requests.requestStatus[0].Load(),
		"1xx":  p.requests.requestStatus[1].Load(),
		"2xx":  p.requests.requestStatus[2].Load(),
		"3xx":  p.requests.requestStatus[3].Load(),
		"4xx":  p.requests.requestStatus[4].Load(),
		"5xx":  p.requests.requestStatus[5].Load(),
	}
	_, durationSum, durationCount := p.duration.snapshot()
	_, ttftSum, ttftCount := p.ttft.snapshot()
	var auditDrops uint64
	if p.auditDrops != nil {
		auditDrops = p.auditDrops()
	}
	return Snapshot{
		Requests: p.requests.requests.Load(), Attempts: p.requests.attempts.Load(),
		Retried: p.requests.retried.Load(), InFlight: p.inFlight.Load(),
		ResponseBytes: p.requests.bytes.Load(), InputTokens: p.requests.input.Load(),
		OutputTokens: p.requests.output.Load(), CachedTokens: p.requests.cached.Load(),
		CacheCreated: p.requests.created.Load(), AuditDrops: auditDrops,
		Outcomes: outcomes, StatusClasses: statusClasses,
		DurationCount: durationCount, DurationSumMs: float64(durationSum) / 1_000_000,
		TTFTCount: ttftCount, TTFTSumMs: float64(ttftSum) / 1_000_000,
	}
}

// ServeHTTP emits a small, dependency-free Prometheus text exposition. The
// endpoint is intentionally global/low-cardinality; detailed model and
// upstream dimensions stay in the database-backed admin views.
func (p *Passive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Prometheus only needs read methods; do not let the diagnostics listener
	// become an accidental write endpoint.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(p.prometheus()))
	}
}

func (p *Passive) prometheus() string {
	var b strings.Builder
	writeHelpType(&b, "muxapi_requests_total", "Completed client requests observed by outcome.", "counter")
	for i, outcome := range outcomeNames {
		fmt.Fprintf(&b, "muxapi_requests_total{outcome=%s} %d\n", quoteLabel(outcome), p.requests.requestOutcome[i].Load())
	}
	writeHelpType(&b, "muxapi_upstream_attempts_total", "Upstream attempts observed by outcome.", "counter")
	for i, outcome := range outcomeNames {
		fmt.Fprintf(&b, "muxapi_upstream_attempts_total{outcome=%s} %d\n", quoteLabel(outcome), p.requests.attemptOutcome[i].Load())
	}
	writeHelpType(&b, "muxapi_requests_status_total", "Completed client requests observed by HTTP status class.", "counter")
	for i, class := range [...]string{"none", "1xx", "2xx", "3xx", "4xx", "5xx"} {
		fmt.Fprintf(&b, "muxapi_requests_status_total{class=%s} %d\n", quoteLabel(class), p.requests.requestStatus[i].Load())
	}
	writeHelpType(&b, "muxapi_requests_retried_total", "Completed client requests with more than one upstream attempt.", "counter")
	fmt.Fprintf(&b, "muxapi_requests_retried_total %d\n", p.requests.retried.Load())
	writeHelpType(&b, "muxapi_requests_in_flight", "Client requests currently being observed.", "gauge")
	fmt.Fprintf(&b, "muxapi_requests_in_flight %d\n", p.inFlight.Load())
	writeHelpType(&b, "muxapi_response_bytes_total", "Client response bytes observed.", "counter")
	fmt.Fprintf(&b, "muxapi_response_bytes_total %d\n", p.requests.bytes.Load())
	writeHelpType(&b, "muxapi_input_tokens_total", "Input tokens observed in completed client requests.", "counter")
	fmt.Fprintf(&b, "muxapi_input_tokens_total %d\n", p.requests.input.Load())
	writeHelpType(&b, "muxapi_output_tokens_total", "Output tokens observed in completed client requests.", "counter")
	fmt.Fprintf(&b, "muxapi_output_tokens_total %d\n", p.requests.output.Load())
	writeHelpType(&b, "muxapi_cached_tokens_total", "Cached input tokens observed in completed client requests.", "counter")
	fmt.Fprintf(&b, "muxapi_cached_tokens_total %d\n", p.requests.cached.Load())
	writeHelpType(&b, "muxapi_cache_creation_tokens_total", "Cache creation tokens observed in completed client requests.", "counter")
	fmt.Fprintf(&b, "muxapi_cache_creation_tokens_total %d\n", p.requests.created.Load())
	writeHelpType(&b, "muxapi_request_audit_dropped_total", "Request audit records dropped by the bounded writer or failed in the database.", "counter")
	auditDrops := uint64(0)
	if p.auditDrops != nil {
		auditDrops = p.auditDrops()
	}
	fmt.Fprintf(&b, "muxapi_request_audit_dropped_total %d\n", auditDrops)
	writeHistogram(&b, "muxapi_request_duration_seconds", "Completed client request duration.", &p.duration)
	writeHistogram(&b, "muxapi_request_ttft_seconds", "Completed client request time to first token/byte.", &p.ttft)
	return b.String()
}

func writeHistogram(b *strings.Builder, name, help string, h *histogram) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	buckets, sumNanos, count := h.snapshot()
	var cumulative uint64
	for i, bound := range histogramBounds {
		cumulative += buckets[i]
		fmt.Fprintf(b, "%s_bucket{le=%s} %d\n", name, strconv.Quote(strconv.FormatFloat(bound, 'g', -1, 64)), cumulative)
	}
	cumulative += buckets[requestDurationBuckets]
	fmt.Fprintf(b, "%s_bucket{le=%s} %d\n%s_count %d\n%s_sum %s\n", name, strconv.Quote("+Inf"), cumulative, name, count, name, strconv.FormatFloat(float64(sumNanos)/1_000_000_000, 'g', -1, 64))
}

func writeHelpType(b *strings.Builder, name, help, kind string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func quoteLabel(value string) string { return strconv.Quote(value) }

func outcomeIndex(outcome string) int {
	for i, name := range outcomeNames {
		if outcome == name {
			return i
		}
	}
	return len(outcomeNames) - 1
}

func statusIndex(status int) int {
	switch {
	case status >= 100 && status < 200:
		return 1
	case status >= 200 && status < 300:
		return 2
	case status >= 300 && status < 400:
		return 3
	case status >= 400 && status < 500:
		return 4
	case status >= 500:
		return 5
	default:
		return 0
	}
}

func nonNegative(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func decrementNonNegative(value *atomic.Int64) {
	for {
		current := value.Load()
		if current <= 0 {
			return
		}
		if value.CompareAndSwap(current, current-1) {
			return
		}
	}
}
