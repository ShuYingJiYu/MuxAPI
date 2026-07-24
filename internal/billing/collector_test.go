package billing

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestFetchSub2API(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing provider authorization")
			return
		}
		switch r.URL.Path {
		case "/v1/usage":
			w.Write([]byte(`{"balance":24.93248518,"remaining":24.93248518,"unit":"USD","isValid":true,"usage":{"total":{"cost":101.4511718,"actual_cost":15.036224541}}}`))
		case "/v1/sub2api/billing":
			w.Write([]byte(`{"object":"sub2api.key_billing","group_rate_multiplier":0.155,"resolved_rate_multiplier":0.155,"effective_rate_multiplier":0.155,"observed_at":"2026-07-24T08:18:28Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, APIKey: "sk-test", BillingType: upstream.BillingSub2API,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Remaining == nil || *result.Remaining != 24.93248518 ||
		result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.155 ||
		result.ReportedListCost == nil || *result.ReportedListCost != 101.4511718 ||
		result.ReportedActualCost == nil || *result.ReportedActualCost != 15.036224541 {
		t.Fatalf("unexpected Sub2API result: %+v", result)
	}
}

func TestFetchNewAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"code":true,"data":{"object":"token_usage","total_used":2864211,"total_available":-2864211,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"success":true,"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			w.Write([]byte(`{"success":true,"data":[{"group":"OAI-PRO20X","other":"{\"group_ratio\":0.18,\"user_group_ratio\":-1}"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, APIKey: "sk-test", BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Unlimited || result.Remaining != nil || result.BillingGroup != "OAI-PRO20X" ||
		result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.18 ||
		result.ReportedActualCost == nil || math.Abs(*result.ReportedActualCost-5.728422) > 0.000001 {
		t.Fatalf("unexpected New API result: %+v", result)
	}
}

func TestFetchNewAPIPartialWithoutLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":100,"total_available":250000,"unlimited_quota":false}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			http.Error(w, "logs disabled", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := Fetch(context.Background(), &upstream.Upstream{
		BaseURL: server.URL, BillingType: upstream.BillingNewAPI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Remaining == nil || *result.Remaining != 0.5 || result.Warning == "" {
		t.Fatalf("partial New API data should retain balance: %+v", result)
	}
}
