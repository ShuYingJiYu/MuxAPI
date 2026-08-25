package store

import (
	"math"
	"testing"
)

func TestOverviewUsageCostReportsCoverage(t *testing.T) {
	st, upstream := newUsageTestStore(t, "overview-summary.db")
	inputCost := 0.01
	outputCost := 0.02
	if err := st.ReplaceModelPricing([]ModelPricing{{
		Model: "gpt-priced", InputCostPerToken: &inputCost, OutputCostPerToken: &outputCost,
	}}, PricingCatalogStatus{Source: "litellm", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	saveTestAttempt(t, st, "req-priced", upstream.ID, "gpt-priced", "openai", "success",
		1_700_000_100, 100, 20, 0)
	saveTestAttempt(t, st, "req-missing-price", upstream.ID, "gpt-unknown", "openai", "success",
		1_700_000_200, 50, 10, 0)

	estimate, err := st.OverviewUsageCost(1_700_000_000, 1_700_000_300)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.Amount == nil || math.Abs(*estimate.Amount-1.4) > 1e-9 {
		t.Fatalf("unexpected estimate amount: %+v", estimate)
	}
	if estimate.RequestCount != 2 || estimate.PricedRequestCount != 1 || estimate.Coverage != 0.5 {
		t.Fatalf("unexpected estimate coverage: %+v", estimate)
	}
	if estimate.Currency != "USD" || estimate.PricingSource != "litellm" {
		t.Fatalf("unexpected estimate metadata: %+v", estimate)
	}
}
