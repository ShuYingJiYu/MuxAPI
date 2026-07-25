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

func TestFetchSub2APIPreservesNegativeBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.Write([]byte(`{"balance":-1.25,"remaining":-1.25,"unit":"USD","isValid":false}`))
		case "/v1/sub2api/billing":
			w.Write([]byte(`{"object":"sub2api.key_billing","group_rate_multiplier":0.2,"effective_rate_multiplier":0.2}`))
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
	if result.Unlimited || result.Remaining == nil || *result.Remaining != -1.25 {
		t.Fatalf("negative Sub2API balance must remain visible: %+v", result)
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

// 关键回归：New API 的 /api/log/token 把错误日志(type=5)和消费日志(type=2)混在
// 一起返回，错误日志的 other 里没有 group_ratio。生产上 mosshubs 有 39% 是错误
// 日志，取「最新一条」会让倍率随机落到分组表的公示价，与实际扣费价不符。
func TestFetchNewAPISkipsErrorLogsWhenReadingMultiplier(t *testing.T) {
	var groupsHit int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":500000,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			// 最新两条是错误日志：带 group 但 other 里只有 error_code/status_code
			w.Write([]byte(`{"success":true,"data":[
				{"group":"OAI-PRO20X","other":"{\"error_code\":\"upstream_error\",\"status_code\":503}"},
				{"group":"OAI-PRO20X","other":"{\"error_code\":\"upstream_error\",\"status_code\":429}"},
				{"group":"OAI-PRO20X","other":"{\"group_ratio\":0.18,\"user_group_ratio\":-1}"}
			]}`))
		case "/api/user/groups":
			groupsHit++
			w.Write([]byte(`{"success":true,"data":{"OAI-PRO20X":{"ratio":0.15}}}`))
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
	if result.BillingGroup != "OAI-PRO20X" {
		t.Fatalf("error logs still carry the group name: %+v", result)
	}
	if result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.18 {
		t.Fatalf("multiplier must come from the newest consumption log, got %v", result.EffectiveMultiplier)
	}
	if groupsHit != 0 {
		t.Fatalf("group table must not be consulted when a consumption log exists")
	}
	if result.Warning != "" {
		t.Fatalf("unexpected warning: %s", result.Warning)
	}
}

// 只有错误日志时才回落到分组公示价，并说明该值未经实际扣费验证。
func TestFetchNewAPIFallsBackToGroupTableWithoutConsumptionLog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			w.Write([]byte(`{"data":{"object":"token_usage","total_used":0,"unlimited_quota":true}}`))
		case "/api/status":
			w.Write([]byte(`{"data":{"quota_per_unit":500000}}`))
		case "/api/log/token":
			w.Write([]byte(`{"success":true,"data":[
				{"group":"claude-kiro","other":"{\"error_code\":\"upstream_error\",\"status_code\":503}"}
			]}`))
		case "/api/user/groups":
			w.Write([]byte(`{"success":true,"data":{"claude-kiro":{"ratio":0.15}}}`))
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
	if result.EffectiveMultiplier == nil || *result.EffectiveMultiplier != 0.15 {
		t.Fatalf("group table must supply the fallback multiplier, got %v", result.EffectiveMultiplier)
	}
	if result.Warning == "" {
		t.Fatal("a list-price multiplier must be reported as unverified")
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
