package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mirainya/muxapi/internal/store"
)

const testPricingDocument = `{
	"gpt-test":{"input_cost_per_token":0.001,"output_cost_per_token":0.002,
		"cache_read_input_token_cost":0.0002,"cache_creation_input_token_cost":0.00125},
	"metadata-only":{"max_tokens":1000}
}`

func TestParseEmbeddedPricingCatalog(t *testing.T) {
	prices, version, err := parsePricingCatalog(embeddedPricingCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) < 1000 || len(version) != 64 {
		t.Fatalf("unexpected embedded catalog: models=%d version=%q", len(prices), version)
	}
}

func TestRefreshPricingInstallsRemoteCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testPricingDocument))
	}))
	defer server.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "pricing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := NewManager(st)
	m.pricingURL = server.URL
	if err := m.refreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := st.GetPricingCatalogStatus()
	if err != nil || status.Source != "LiteLLM" || status.ModelCount != 1 || status.Version == "" {
		t.Fatalf("unexpected pricing status: %+v, err=%v", status, err)
	}
	price, err := st.LookupModelPricing("openai/gpt-test")
	if err != nil || price.InputCostPerToken == nil || *price.InputCostPerToken != 0.001 {
		t.Fatalf("unexpected model price: %+v, err=%v", price, err)
	}
}

func TestRefreshPricingKeepsLastSuccessfulCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "pricing-retain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prices, version, err := parsePricingCatalog([]byte(testPricingDocument))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceModelPricing(prices, store.PricingCatalogStatus{
		Source: "LiteLLM", Version: version, LastCheckedAt: 1, LastSuccessAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st)
	m.pricingURL = server.URL
	if err := m.refreshPricing(context.Background()); err == nil {
		t.Fatal("failed remote refresh should return an error")
	}
	status, err := st.GetPricingCatalogStatus()
	if err != nil || status.Version != version || status.ModelCount != 1 || status.Error == "" {
		t.Fatalf("last successful catalog was not retained: %+v, err=%v", status, err)
	}
	if _, err := st.LookupModelPricing("gpt-test"); err != nil {
		t.Fatalf("retained model is unavailable: %v", err)
	}
}

func TestRefreshPricingUsesEmbeddedCatalogOnFirstFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pricing-fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := NewManager(st)
	m.pricingURL = "://invalid"
	m.pricingFallback = []byte(testPricingDocument)
	if err := m.refreshPricing(context.Background()); err == nil {
		t.Fatal("remote failure should still be observable")
	}
	status, err := st.GetPricingCatalogStatus()
	if err != nil || status.Source != "LiteLLM embedded" || status.ModelCount != 1 || status.Error == "" {
		t.Fatalf("embedded catalog was not installed: %+v, err=%v", status, err)
	}
	if _, err := st.LookupModelPricing("gpt-test"); err != nil {
		t.Fatalf("embedded model is unavailable: %v", err)
	}
}
