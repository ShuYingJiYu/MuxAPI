package billing

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

const (
	defaultPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	maxPricingBytes   = 32 << 20
)

//go:embed litellm_prices.json
var embeddedPricingCatalog []byte

type liteLLMPrice struct {
	InputCostPerToken        *float64 `json:"input_cost_per_token"`
	OutputCostPerToken       *float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost  *float64 `json:"cache_read_input_token_cost"`
	CacheWriteInputTokenCost *float64 `json:"cache_creation_input_token_cost"`
}

func parsePricingCatalog(data []byte) ([]store.ModelPricing, string, error) {
	if len(data) == 0 || len(data) > maxPricingBytes {
		return nil, "", fmt.Errorf("pricing catalog size %d is invalid", len(data))
	}
	var document map[string]liteLLMPrice
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, "", fmt.Errorf("decode pricing catalog: %w", err)
	}
	prices := make([]store.ModelPricing, 0, len(document))
	for model, price := range document {
		if strings.TrimSpace(model) == "" || (price.InputCostPerToken == nil &&
			price.OutputCostPerToken == nil && price.CacheReadInputTokenCost == nil &&
			price.CacheWriteInputTokenCost == nil) {
			continue
		}
		prices = append(prices, store.ModelPricing{
			Model: model, InputCostPerToken: price.InputCostPerToken,
			OutputCostPerToken:       price.OutputCostPerToken,
			CacheReadInputTokenCost:  price.CacheReadInputTokenCost,
			CacheWriteInputTokenCost: price.CacheWriteInputTokenCost,
		})
	}
	if len(prices) == 0 {
		return nil, "", errors.New("pricing catalog contains no usable models")
	}
	digest := sha256.Sum256(data)
	return prices, hex.EncodeToString(digest[:]), nil
}

func (m *Manager) downloadPricingCatalog(ctx context.Context) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.pricingURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MuxAPI pricing-sync")
	response, err := m.pricingClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("pricing catalog returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPricingBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPricingBytes {
		return nil, errors.New("pricing catalog exceeds 32 MiB")
	}
	return data, nil
}

func (m *Manager) installPricingCatalog(data []byte, source, warning string, now time.Time) error {
	prices, version, err := parsePricingCatalog(data)
	if err != nil {
		return err
	}
	return m.store.ReplaceModelPricing(prices, store.PricingCatalogStatus{
		Source: source, Version: version, ModelCount: len(prices),
		LastCheckedAt: now.Unix(), LastSuccessAt: now.Unix(), Error: warning,
	})
}

// refreshPricing installs the remote catalog, retaining or bootstrapping cached data on failure.
func (m *Manager) refreshPricing(ctx context.Context) error {
	now := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, m.pricingTimeout)
	defer cancel()
	data, downloadErr := m.downloadPricingCatalog(requestCtx)
	if downloadErr == nil {
		if err := m.installPricingCatalog(data, "LiteLLM", "", now); err == nil {
			return nil
		} else {
			downloadErr = err
		}
	}

	status, statusErr := m.store.GetPricingCatalogStatus()
	if statusErr == nil && status.ModelCount > 0 {
		if err := m.store.SavePricingCatalogFailure(downloadErr.Error(), now.Unix()); err != nil {
			return errors.Join(downloadErr, err)
		}
		return downloadErr
	}
	if err := m.installPricingCatalog(m.pricingFallback, "LiteLLM embedded", downloadErr.Error(), now); err != nil {
		failure := errors.Join(downloadErr, fmt.Errorf("load embedded pricing catalog: %w", err))
		_ = m.store.SavePricingCatalogFailure(failure.Error(), now.Unix())
		return failure
	}
	return downloadErr
}
