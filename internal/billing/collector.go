// Package billing reads provider-side balances, multipliers, and cumulative costs.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

const maxBillingResponse = 2 << 20

var ErrBillingDisabled = errors.New("billing collection is disabled for this upstream")

// Result is a normalized provider billing snapshot. Nil numbers are unavailable,
// while zero values from the provider remain distinguishable and meaningful.
type Result struct {
	Currency            string
	Remaining           *float64
	Unlimited           bool
	BillingGroup        string
	GroupMultiplier     *float64
	EffectiveMultiplier *float64
	ReportedListCost    *float64
	ReportedActualCost  *float64
	ObservedAt          time.Time
	Warning             string
}

// Fetch selects the configured provider adapter. The caller owns timeout and scheduling.
func Fetch(ctx context.Context, item *upstream.Upstream) (Result, error) {
	switch item.BillingType {
	case upstream.BillingSub2API:
		return fetchSub2API(ctx, item)
	case upstream.BillingNewAPI:
		return fetchNewAPI(ctx, item)
	default:
		return Result{}, ErrBillingDisabled
	}
}

func getJSON(ctx context.Context, item *upstream.Upstream, path string, target any) error {
	req, err := item.BuildRequest(http.MethodGet, path, nil, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)
	resp, err := (&http.Client{Transport: item.NewTransport()}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBillingResponse+1))
	if err != nil {
		return err
	}
	if len(body) > maxBillingResponse {
		return errors.New("billing response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if len(message) > 512 {
			message = message[:512]
		}
		return fmt.Errorf("billing endpoint %s returned %d: %s", path, resp.StatusCode, message)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode billing endpoint %s: %w", path, err)
	}
	return nil
}

func floatPtr(value float64) *float64 { return &value }

type sub2UsageResponse struct {
	Balance   *float64 `json:"balance"`
	Remaining *float64 `json:"remaining"`
	Unit      string   `json:"unit"`
	IsValid   bool     `json:"isValid"`
	Usage     struct {
		Total struct {
			Cost       *float64 `json:"cost"`
			ActualCost *float64 `json:"actual_cost"`
		} `json:"total"`
	} `json:"usage"`
}

type sub2BillingResponse struct {
	Object                  string  `json:"object"`
	GroupRateMultiplier     float64 `json:"group_rate_multiplier"`
	ResolvedRateMultiplier  float64 `json:"resolved_rate_multiplier"`
	EffectiveRateMultiplier float64 `json:"effective_rate_multiplier"`
	ObservedAt              string  `json:"observed_at"`
}

func fetchSub2API(ctx context.Context, item *upstream.Upstream) (Result, error) {
	var usage sub2UsageResponse
	if err := getJSON(ctx, item, "/v1/usage", &usage); err != nil {
		return Result{}, err
	}
	var provider sub2BillingResponse
	if err := getJSON(ctx, item, "/v1/sub2api/billing", &provider); err != nil {
		return Result{}, err
	}
	if provider.Object != "sub2api.key_billing" {
		return Result{}, errors.New("unexpected Sub2API billing response")
	}
	remaining := usage.Remaining
	if remaining == nil {
		remaining = usage.Balance
	}
	observed := time.Now()
	if parsed, err := time.Parse(time.RFC3339Nano, provider.ObservedAt); err == nil {
		observed = parsed
	}
	currency := strings.TrimSpace(usage.Unit)
	if currency == "" {
		currency = "USD"
	}
	return Result{
		Currency: currency, Remaining: remaining,
		GroupMultiplier:     floatPtr(provider.GroupRateMultiplier),
		EffectiveMultiplier: floatPtr(provider.EffectiveRateMultiplier),
		ReportedListCost:    usage.Usage.Total.Cost, ReportedActualCost: usage.Usage.Total.ActualCost,
		ObservedAt: observed,
	}, nil
}

type newAPIUsageResponse struct {
	Data struct {
		Object         string  `json:"object"`
		TotalUsed      float64 `json:"total_used"`
		TotalAvailable float64 `json:"total_available"`
		UnlimitedQuota bool    `json:"unlimited_quota"`
	} `json:"data"`
}

type newAPIStatusResponse struct {
	Data struct {
		QuotaPerUnit float64 `json:"quota_per_unit"`
	} `json:"data"`
}

type newAPILogResponse struct {
	Success bool `json:"success"`
	Data    []struct {
		Group string          `json:"group"`
		Other json.RawMessage `json:"other"`
	} `json:"data"`
}

type newAPILogBilling struct {
	GroupRatio     *float64 `json:"group_ratio"`
	UserGroupRatio *float64 `json:"user_group_ratio"`
}

type newAPIGroupResponse struct {
	Data map[string]struct {
		Ratio json.RawMessage `json:"ratio"`
	} `json:"data"`
}

func decodeNewAPILogBilling(raw json.RawMessage, target *newAPILogBilling) bool {
	if len(raw) == 0 {
		return false
	}
	if json.Unmarshal(raw, target) == nil {
		return true
	}
	var encoded string
	return json.Unmarshal(raw, &encoded) == nil && json.Unmarshal([]byte(encoded), target) == nil
}

func fetchNewAPI(ctx context.Context, item *upstream.Upstream) (Result, error) {
	var usage newAPIUsageResponse
	if err := getJSON(ctx, item, "/api/usage/token/", &usage); err != nil {
		return Result{}, err
	}
	if usage.Data.Object != "token_usage" {
		return Result{}, errors.New("unexpected New API usage response")
	}
	var status newAPIStatusResponse
	if err := getJSON(ctx, item, "/api/status", &status); err != nil {
		return Result{}, err
	}
	if status.Data.QuotaPerUnit <= 0 {
		return Result{}, errors.New("New API returned invalid quota_per_unit")
	}

	result := Result{Currency: "USD", Unlimited: usage.Data.UnlimitedQuota, ObservedAt: time.Now()}
	if !result.Unlimited {
		remaining := math.Max(0, usage.Data.TotalAvailable/status.Data.QuotaPerUnit)
		result.Remaining = &remaining
	}
	actual := usage.Data.TotalUsed / status.Data.QuotaPerUnit
	result.ReportedActualCost = &actual

	var logs newAPILogResponse
	if err := getJSON(ctx, item, "/api/log/token", &logs); err != nil {
		result.Warning = err.Error()
		return result, nil
	}
	for _, entry := range logs.Data {
		if strings.TrimSpace(entry.Group) == "" {
			continue
		}
		result.BillingGroup = entry.Group
		var detail newAPILogBilling
		if decodeNewAPILogBilling(entry.Other, &detail) {
			result.GroupMultiplier = detail.GroupRatio
			result.EffectiveMultiplier = detail.GroupRatio
			if detail.UserGroupRatio != nil && *detail.UserGroupRatio >= 0 {
				result.EffectiveMultiplier = detail.UserGroupRatio
			}
		}
		break
	}
	if result.BillingGroup == "" {
		result.Warning = "New API has no recent token log for billing group detection"
		return result, nil
	}
	if result.GroupMultiplier == nil {
		var groups newAPIGroupResponse
		if err := getJSON(ctx, item, "/api/user/groups", &groups); err != nil {
			result.Warning = err.Error()
			return result, nil
		}
		if group, ok := groups.Data[result.BillingGroup]; ok {
			var ratio float64
			if json.Unmarshal(group.Ratio, &ratio) == nil {
				result.GroupMultiplier = &ratio
				result.EffectiveMultiplier = &ratio
			}
		}
	}
	if result.EffectiveMultiplier == nil {
		result.Warning = "New API billing multiplier is unavailable"
	}
	return result, nil
}
