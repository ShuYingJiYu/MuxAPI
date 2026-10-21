package forward

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestAllOpenFailureDoesNotBlockOtherChannelInSameRequest(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) }))
	defer good.Close()
	channels := []*upstream.Upstream{{ID: 1, BaseURL: bad.URL, Protocol: "passthrough", Priority: 1}, {ID: 2, BaseURL: good.URL, Protocol: "passthrough", Priority: 2}}
	manager := health.New(1, 10*time.Minute)
	manager.Report(1, "model", false, 0)
	manager.Report(2, "model", false, 0)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return channels }, manager), manager, 2)
	body := []byte(`{"model":"model"}`)
	result := fwd.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")
	if result.Outcome != OutcomeSuccess || result.FinalUpstreamID != 2 || len(result.Attempts) != 2 {
		t.Fatalf("one failed recovery blocked the other channel: %+v", result)
	}
}

func TestUnavailableReturnsRetryAfterAndSpecificReason(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(strconv.FormatBool(busy), func(t *testing.T) {
			manager := health.New(3, time.Minute)
			lease, _ := manager.Claim(1, 7, "model")
			want := "upstream_rate_limit"
			if busy {
				manager.CompleteModelTransientFailure(lease, 5, 200, "overloaded")
				trial, ok := manager.ClaimLastResort(2, 7, "model")
				if !ok {
					t.Fatal("recovery claim failed")
				}
				defer manager.Release(trial)
				want = "recovery_in_progress"
			} else {
				manager.CompleteModelRateLimited(lease, 5, time.Minute, 429, "limited")
			}
			channels := []*upstream.Upstream{{ID: 7, BaseURL: "http://127.0.0.1:1", Protocol: "passthrough"}}
			fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return channels }, manager), manager, 1)
			body := []byte(`{"model":"model"}`)
			recorder := httptest.NewRecorder()
			result := fwd.Forward(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, 1, "")
			retry, _ := strconv.Atoi(recorder.Header().Get("Retry-After"))
			if result.Status != 503 || result.ErrorKind != want || retry < 1 || len(result.Attempts) != 0 {
				t.Fatalf("missing useful unavailability reason: %+v retry=%d", result, retry)
			}
			if !busy && retry < 59 {
				t.Fatalf("Retry-After shortened the explicit rate limit: %d", retry)
			}
		})
	}
}

func TestCancelWhileReadingFailureHeadersReleasesWithoutHealthPenalty(t *testing.T) {
	for _, status := range []int{429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			headers := make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				close(headers)
				<-r.Context().Done()
			}))
			defer provider.Close()
			channels := []*upstream.Upstream{{ID: 7, BaseURL: provider.URL, Protocol: "passthrough"}}
			manager := health.New(1, time.Minute)
			fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return channels }, manager), manager, 1)
			body := []byte(`{"model":"model"}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan Result, 1)
			go func() {
				finished <- fwd.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx), body, 1, "")
			}()
			<-headers
			cancel()
			select {
			case result := <-finished:
				if result.Outcome != OutcomeCanceled || manager.InFlight(7) != 0 || len(manager.ModelStates(7)) != 0 || manager.Snapshot(7).Fails != 0 {
					t.Fatalf("cancellation leaked recovery or changed health: %+v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceled attempt did not finish")
			}
		})
	}
}
