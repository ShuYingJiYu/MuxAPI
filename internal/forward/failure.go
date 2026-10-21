package forward

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/translate"
	"github.com/mirainya/muxapi/internal/upstream"
)

func completeResponseFailure(h Health, lease health.Lease, failure *upstream.ResponseError, latency int64, retryAfter time.Duration) {
	switch failure.Kind {
	case upstream.FailureClient:
		h.Complete(lease, health.ResultNeutral, 0)
	case upstream.FailureUnsupported:
		if detailed, ok := h.(modelUnsupportedHealth); ok {
			detailed.CompleteModelUnsupported(lease, latency, failure.Status, failure.Error())
		} else {
			h.Complete(lease, health.ResultModelUnsupported, latency)
		}
	case upstream.FailureRateLimit:
		if scoped, ok := h.(modelRateLimitedHealth); ok {
			scoped.CompleteModelRateLimited(lease, latency, retryAfter, failure.Status, failure.Error())
		} else {
			h.Complete(lease, health.ResultScopedFailure, latency)
		}
	case upstream.FailureTransient:
		if scoped, ok := h.(modelTransientHealth); ok {
			scoped.CompleteModelTransientFailure(lease, latency, failure.Status, failure.Error())
		} else {
			h.Complete(lease, health.ResultScopedFailure, latency)
		}
	default:
		h.Complete(lease, health.ResultFailure, latency)
	}
}

func writeClientFailure(w http.ResponseWriter, failure *upstream.ResponseError, source translate.Format) {
	status := failure.Status
	if status < 400 || status >= 500 {
		status = http.StatusBadRequest
	}
	payload, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": "invalid_request_error", "code": failure.Code, "message": failure.Error(),
	}})
	payload = translate.ErrorResponse(source, status, payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func writeUnavailable(w http.ResponseWriter, err error) (string, string) {
	kind, message := "no_upstream", "no upstream available"
	var unavailable *scheduler.UnavailableError
	if errors.As(err, &unavailable) {
		kind = unavailable.Reason
		switch kind {
		case "recovery_in_progress":
			message = "upstream recovery is in progress; retry shortly"
		case "upstream_rate_limit":
			message = "upstreams are rate limited; retry after the cooldown"
		case "model_unsupported":
			message = "model availability is awaiting revalidation"
		default:
			message = "upstreams are temporarily unavailable; recovery is cooling down"
		}
		seconds := max(1, int(math.Ceil(time.Until(unavailable.RetryAt).Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"type": "upstream_unavailable", "code": kind, "message": message,
	}})
	return kind, message
}
