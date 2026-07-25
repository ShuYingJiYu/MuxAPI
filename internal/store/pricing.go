package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ModelPricing contains USD prices per token from the local LiteLLM catalog.
type ModelPricing struct {
	Model                    string
	InputCostPerToken        *float64
	OutputCostPerToken       *float64
	CacheReadInputTokenCost  *float64
	CacheWriteInputTokenCost *float64
}

// PricingCatalogStatus describes the most recent catalog refresh attempt.
type PricingCatalogStatus struct {
	Source        string `json:"source"`
	Version       string `json:"version"`
	ModelCount    int    `json:"model_count"`
	LastCheckedAt int64  `json:"last_checked_at,omitempty"`
	LastSuccessAt int64  `json:"last_success_at,omitempty"`
	Error         string `json:"error,omitempty"`
}

// ReplaceModelPricing atomically installs one complete catalog version.
func (s *Store) ReplaceModelPricing(prices []ModelPricing, status PricingCatalogStatus) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM model_pricing`); err != nil {
		return err
	}
	for _, price := range prices {
		if strings.TrimSpace(price.Model) == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO model_pricing(
			model,input_cost_per_token,output_cost_per_token,cache_read_input_token_cost,
			cache_creation_input_token_cost) VALUES(?,?,?,?,?)`,
			price.Model, price.InputCostPerToken, price.OutputCostPerToken,
			price.CacheReadInputTokenCost, price.CacheWriteInputTokenCost); err != nil {
			return err
		}
	}
	now := time.Now()
	checked := billingTimestamp(status.LastCheckedAt, now)
	succeeded := billingTimestamp(status.LastSuccessAt, now)
	_, err = tx.Exec(`INSERT INTO pricing_catalog_status(
		id,source,version,model_count,last_checked_at,last_success_at,error_text)
		VALUES(1,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
		source=excluded.source,version=excluded.version,model_count=excluded.model_count,
		last_checked_at=excluded.last_checked_at,last_success_at=excluded.last_success_at,
		error_text=excluded.error_text`, status.Source, status.Version, len(prices),
		s.timeValue(checked), s.timeValue(succeeded), status.Error)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SavePricingCatalogFailure records a refresh failure without changing cached prices.
func (s *Store) SavePricingCatalogFailure(message string, checkedAt int64) error {
	checked := billingTimestamp(checkedAt, time.Now())
	_, err := s.db.Exec(`INSERT INTO pricing_catalog_status(
		id,source,version,model_count,last_checked_at,error_text) VALUES(1,'','',0,?,?)
		ON CONFLICT(id) DO UPDATE SET last_checked_at=excluded.last_checked_at,
		error_text=excluded.error_text`, s.timeValue(checked), message)
	return err
}

// GetPricingCatalogStatus returns the currently installed catalog metadata.
func (s *Store) GetPricingCatalogStatus() (PricingCatalogStatus, error) {
	var status PricingCatalogStatus
	err := s.db.QueryRow(`SELECT source,version,model_count,`+
		s.billingTimeExpr("last_checked_at")+`,`+s.billingTimeExpr("last_success_at")+`,error_text
		FROM pricing_catalog_status WHERE id=1`).Scan(&status.Source, &status.Version,
		&status.ModelCount, &status.LastCheckedAt, &status.LastSuccessAt, &status.Error)
	return status, err
}

func pricingModelCandidates(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	candidates := []string{model}
	trimmed := strings.TrimPrefix(model, "models/")
	if trimmed != model {
		candidates = append(candidates, trimmed)
	}
	if slash := strings.IndexByte(trimmed, '/'); slash >= 0 && slash+1 < len(trimmed) {
		bare := trimmed[slash+1:]
		seen := false
		for _, candidate := range candidates {
			seen = seen || candidate == bare
		}
		if !seen {
			candidates = append(candidates, bare)
		}
	}
	return candidates
}

// LookupModelPricing matches an exact model first, then common provider prefixes.
func (s *Store) LookupModelPricing(model string) (ModelPricing, error) {
	for _, candidate := range pricingModelCandidates(model) {
		var price ModelPricing
		err := s.db.QueryRow(`SELECT model,input_cost_per_token,output_cost_per_token,
			cache_read_input_token_cost,cache_creation_input_token_cost
			FROM model_pricing WHERE model=?`, candidate).Scan(&price.Model,
			&price.InputCostPerToken, &price.OutputCostPerToken,
			&price.CacheReadInputTokenCost, &price.CacheWriteInputTokenCost)
		if err == nil {
			return price, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ModelPricing{}, err
		}
	}
	return ModelPricing{}, sql.ErrNoRows
}

// billingCountsCanceled 控制客户端主动断开的尝试是否计入用量。
// 上游在客户端断连后是否继续生成、是否照收，各家实现不同：有的立刻中止只收已
// 生成部分，有的跑完全程照收。默认不计入（偏保守，宁可低估本地理论费用也不
// 凭猜测加账）；若确认上游照收，改为 true 可提高比对准确度。
const billingCountsCanceled = false

// BillingWindowUsage groups billed request attempts for local cost calculation.
type BillingWindowUsage struct {
	Model               string
	Protocol            string
	RequestCount        int64
	MissingUsageCount   int64
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	CacheCreationTokens int64
}

// ListBillingWindowUsage returns successful attempts in the half-open snapshot window.
func (s *Store) ListBillingWindowUsage(upstreamID, fromAt, toAt int64) ([]BillingWindowUsage, error) {
	// 协议优先取 attempt 行上的快照；历史行为空时回退到现查（与旧行为一致）。
	protocolExpr := `COALESCE(NULLIF(a.protocol,''),u.protocol,'')`
	// outcome 过滤：success 与 partial 都已消耗上游 token（partial=响应已提交后
	// 流中断，上游那边照样生成并计费），只统计 success 会系统性压低理论费用，
	// 把偏差单向推向「上游多收」的误报。canceled 是否计费取决于上游断连后是否
	// 继续生成，无法一概而论，故由 billingCountsCanceled 控制，默认不计入。
	outcomes := "'success','partial'"
	if billingCountsCanceled {
		outcomes = "'success','partial','canceled'"
	}
	// usage 完整性分两种形态：
	//   四项全零 —— 完全没拿到 usage，任何 outcome 下都算不完整；
	//   partial 且无 output —— 流在 usage 事件前断掉，按 output=0 定价会低估。
	// success 且有 input 无 output 不算异常：部分上游只回报 prompt token。
	incompleteExpr := `CASE WHEN (a.input_tokens=0 AND a.output_tokens=0 AND a.cached_tokens=0
			AND a.cache_creation_tokens=0) OR (a.outcome<>'success' AND a.output_tokens=0)
		THEN 1 ELSE 0 END`
	query := `SELECT r.model,` + protocolExpr + `,COUNT(*),
		COALESCE(SUM(` + incompleteExpr + `),0),COALESCE(SUM(a.input_tokens),0),
		COALESCE(SUM(a.output_tokens),0),COALESCE(SUM(a.cached_tokens),0),
		COALESCE(SUM(a.cache_creation_tokens),0)
		FROM request_attempts a
		JOIN requests r ON r.request_id=a.request_id
		LEFT JOIN upstreams u ON u.id=a.upstream_id
		WHERE a.upstream_id=? AND a.outcome IN (` + outcomes + `)
			AND a.completed_at>? AND a.completed_at<=?
		GROUP BY r.model,` + protocolExpr + ` ORDER BY r.model`
	rows, err := s.db.Query(query, upstreamID, s.timeValue(time.Unix(fromAt, 0)),
		s.timeValue(time.Unix(toAt, 0)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BillingWindowUsage
	for rows.Next() {
		var usage BillingWindowUsage
		if err := rows.Scan(&usage.Model, &usage.Protocol, &usage.RequestCount, &usage.MissingUsageCount,
			&usage.InputTokens, &usage.OutputTokens, &usage.CachedTokens,
			&usage.CacheCreationTokens); err != nil {
			return nil, err
		}
		out = append(out, usage)
	}
	return out, rows.Err()
}
