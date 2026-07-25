package store

import (
	"database/sql"
	"errors"
	"math"
	"time"
)

// BillingWindow 是比对可选的时间尺度。单个刷新间隔（默认 10min）样本太小、
// 噪声最大，且结论会被下一轮覆盖，故聚合到小时以上再判定。
type BillingWindow struct {
	Key      string
	Label    string
	Duration time.Duration
}

var billingWindows = []BillingWindow{
	{Key: "1h", Label: "近 1 小时", Duration: time.Hour},
	{Key: "24h", Label: "近 24 小时", Duration: 24 * time.Hour},
	{Key: "7d", Label: "近 7 天", Duration: 7 * 24 * time.Hour},
}

// DefaultBillingWindow 默认尺度：跨过昼夜波动，样本量足以让偏差有统计意义。
const DefaultBillingWindow = "24h"

// BillingWindows 暴露给管理接口做选项。
func BillingWindows() []BillingWindow { return billingWindows }

// LookupBillingWindow 解析窗口键，未知值回落到默认尺度。
func LookupBillingWindow(key string) BillingWindow {
	for _, window := range billingWindows {
		if window.Key == key {
			return window
		}
	}
	for _, window := range billingWindows {
		if window.Key == DefaultBillingWindow {
			return window
		}
	}
	return billingWindows[0]
}

// BillingSnapshotKeepDays 快照保留天数。须显著大于最长聚合窗口(7d)，为「窗口
// 左端点」留余量；每上游每刷新间隔一行，30 天的量很小。
const BillingSnapshotKeepDays = 30

// PruneBillingSnapshots 删除过期快照，但每个上游保底留最近 2 条——
// 否则久无流量的上游会被清空，连即时窗口都算不出来。
func (s *Store) PruneBillingSnapshots(keepDays int) (int64, error) {
	if keepDays <= 0 {
		keepDays = BillingSnapshotKeepDays
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	result, err := s.db.Exec(`DELETE FROM upstream_billing_snapshots WHERE id IN (
		SELECT id FROM (
			SELECT id,observed_at,
				ROW_NUMBER() OVER (PARTITION BY upstream_id ORDER BY observed_at DESC,id DESC) AS snapshot_rank
			FROM upstream_billing_snapshots
		) ranked WHERE snapshot_rank>2 AND observed_at<?
	)`, s.timeValue(cutoff))
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, nil // 驱动不支持行数统计时不视为失败
	}
	return deleted, nil
}

// ListBillingSnapshotsSince 返回窗口内的快照，按时间升序，供相邻对累加增量。
// 额外向前多取一条作为区间左端点，否则窗口内第一条快照的增量无从计算。
func (s *Store) ListBillingSnapshotsSince(upstreamID, sinceAt int64) ([]BillingSnapshot, error) {
	query := `SELECT id,upstream_id,currency,remaining,unlimited,billing_group,group_multiplier,
		effective_multiplier,reported_list_cost,reported_actual_cost,` + s.billingTimeExpr("observed_at") +
		` FROM upstream_billing_snapshots WHERE upstream_id=? AND observed_at>=?
		ORDER BY observed_at ASC,id ASC`
	rows, err := s.db.Query(query, upstreamID, s.timeValue(time.Unix(sinceAt, 0)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BillingSnapshot
	for rows.Next() {
		snapshot, err := scanBillingSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

// billingRangeTotals 是窗口内逐对累加的结果。
// 逐对累加而非首尾相减：计数器重置、中途充值都只污染单个对，不毁掉整个窗口。
type billingRangeTotals struct {
	actual        float64
	actualPairs   int
	reported      float64
	reportedPairs int
	balanceSpent  float64
	balancePairs  int
	resetPairs    int
}

func accumulateBillingRange(snapshots []BillingSnapshot) billingRangeTotals {
	var totals billingRangeTotals
	for i := 1; i < len(snapshots); i++ {
		previous, current := snapshots[i-1], snapshots[i]

		if delta, ok := billingCounterDelta(current.ReportedActualCost, previous.ReportedActualCost); !ok {
			totals.resetPairs++ // 计数器回退：上游侧重置，本对不可用
		} else if delta != nil {
			totals.actual += *delta
			totals.actualPairs++
		}

		if delta, ok := billingCounterDelta(current.ReportedListCost, previous.ReportedListCost); ok && delta != nil {
			totals.reported += *delta
			totals.reportedPairs++
		}

		// 余额只累加下降部分；上升说明中途充值，跳过该对而不是记成负支出。
		if !current.Unlimited && !previous.Unlimited && current.Remaining != nil && previous.Remaining != nil {
			if spent := *previous.Remaining - *current.Remaining; spent >= -1e-9 {
				if spent < 1e-9 {
					spent = 0
				}
				totals.balanceSpent += spent
				totals.balancePairs++
			}
		}
	}
	return totals
}

// multiplierAcrossRange 返回窗口末端倍率，并报告窗口内是否变过。
// 变过时不再直接放弃比对（旧行为），而是标注出来——长窗口里倍率调整很正常，
// 直接放弃会让 7 天尺度几乎永远不可用。
func multiplierAcrossRange(snapshots []BillingSnapshot) (latest *float64, changed bool) {
	for i := len(snapshots) - 1; i >= 0; i-- {
		value := snapshotMultiplier(snapshots[i])
		if value == nil {
			continue
		}
		if latest == nil {
			latest = value
			continue
		}
		if math.Abs(*value-*latest) > 1e-9 {
			changed = true
		}
	}
	return latest, changed
}

// BillingAuditRange 聚合一个时间窗口内的比对结果。
func (s *Store) BillingAuditRange(upstreamID int64, window BillingWindow, now time.Time) (BillingAudit, error) {
	audit := BillingAudit{Status: "pending", Reason: "insufficient_snapshots"}
	audit.WindowSeconds = int64(window.Duration / time.Second)
	if status, err := s.GetPricingCatalogStatus(); err == nil {
		audit.PricingSource = status.Source
		audit.PricingVersion = status.Version
	} else if !errors.Is(err, sql.ErrNoRows) {
		return audit, err
	}

	snapshots, err := s.ListBillingSnapshotsSince(upstreamID, now.Add(-window.Duration).Unix())
	if err != nil {
		return audit, err
	}
	audit.SnapshotCount = len(snapshots)
	if len(snapshots) < 2 {
		return audit, nil
	}
	audit.FromAt = snapshots[0].ObservedAt
	audit.ToAt = snapshots[len(snapshots)-1].ObservedAt

	totals := accumulateBillingRange(snapshots)
	multiplier, changed := multiplierAcrossRange(snapshots)
	audit.ExpectedMultiplier = billingValue(multiplier)
	audit.MultiplierChanged = changed
	if totals.balancePairs > 0 {
		audit.BalanceSpent = &totals.balanceSpent
	}
	if totals.actualPairs > 0 {
		actual := totals.actual
		audit.ActualCost = &actual
		audit.ActualSource = "reported"
	} else if audit.BalanceSpent != nil {
		audit.ActualCost = billingValue(audit.BalanceSpent)
		audit.ActualSource = "balance"
	}
	if totals.reportedPairs > 0 {
		reported := totals.reported
		audit.ReportedListCost = &reported
	}

	if err := s.applyLocalPricing(&audit, upstreamID); err != nil {
		audit.Status = "unavailable"
		audit.Reason = "pricing_query_failed"
		return audit, nil
	}
	if audit.Status == "unavailable" {
		return audit, nil
	}
	evaluateBillingAudit(&audit, multiplier)
	return audit, nil
}

// evaluateBillingAudit 执行双轨判定，是即时窗口与区间聚合共用的收尾。
//
// 轨道一（价目核对）：本地 ListCost 与上游自报原价的差距。这一项只说明两张
// 价目表不一致，不代表上游多收，故只填数值、不触发告警。
//
// 轨道二（计费核对）：ActualCost 与「基准×倍率」的差距。基准优先取上游自报原价
// ——用上游自己的挂牌价算，结论不受本地价表漂移污染，这才是真正查倍率有没有被
// 正确应用。上游不提供原价时（如 newAPI）降级用本地 ListCost，并把 BillingBasis
// 标为 "local" 让调用方降低可信度。
func evaluateBillingAudit(audit *BillingAudit, multiplier *float64) {
	if multiplier == nil {
		audit.Status = "unavailable"
		audit.Reason = "multiplier_unavailable"
		return
	}

	// 轨道一：价目核对。上游自报原价与本地独立估算差太多，说明上游价目表与
	// 公共价目表严重偏离——余额是按上游原价扣的，虚标同样是真金白银的损失，
	// 故超过阈值必须告警，而非只填数值。
	catalogMismatch := false
	if audit.ListCost != nil && audit.ReportedListCost != nil {
		catalogDeviation := *audit.ReportedListCost - *audit.ListCost
		audit.CatalogDeviation = &catalogDeviation
		reference := math.Max(*audit.ListCost, *audit.ReportedListCost)
		if reference > 1e-9 {
			rate := catalogDeviation / reference
			audit.CatalogRate = &rate
			// 只在上游自报「高于」本地估算时告警；低于说明上游给了折扣价，不是问题。
			catalogMismatch = rate > billingCatalogTolerance
		}
	}

	// 轨道二：选定计费基准。
	var basis *float64
	switch {
	case audit.ReportedListCost != nil:
		basis = audit.ReportedListCost
		audit.BillingBasis = "reported"
	case audit.ListCost != nil:
		basis = audit.ListCost
		audit.BillingBasis = "local"
	default:
		audit.Status = "unavailable"
		audit.Reason = "pricing_catalog_unavailable"
		return
	}

	theoretical := *basis * *multiplier
	audit.TheoreticalCost = &theoretical
	if *basis > 1e-9 && audit.ActualCost != nil {
		observed := *audit.ActualCost / *basis
		audit.ObservedMultiplier = &observed
	}
	if audit.ActualCost == nil {
		audit.Status = "unavailable"
		audit.Reason = "actual_cost_unavailable"
		return
	}

	deviation := *audit.ActualCost - theoretical
	audit.Deviation = &deviation
	if theoretical > 1e-9 {
		rate := deviation / theoretical
		audit.DeviationRate = &rate
	}
	tolerance := math.Max(billingAuditAbsoluteTolerance, theoretical*billingAuditRelativeTolerance)
	// 计费核对优先：它是「上游按自己的规则多收了」，比价表偏离更直接。
	if deviation > tolerance {
		audit.Status = "warning"
		audit.Reason = "actual_cost_exceeded"
		return
	}
	if catalogMismatch {
		audit.Status = "warning"
		audit.Reason = "catalog_cost_exceeded"
		return
	}
	audit.Status = "ok"
	audit.Reason = ""
}
