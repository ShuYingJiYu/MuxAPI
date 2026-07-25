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
	// theoretical 用**每对自己的倍率**累加。拿窗口末端单一倍率乘整窗累计原价，
	// 会把窗口内的倍率调整整段计入偏差，产生纯粹的误报。
	theoretical      float64
	theoreticalPairs int
	balanceSpent     float64
	balancePairs     int
	resetPairs       int
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
			// 该区间生效的倍率取本对末端快照的值，与增量归属方式一致。
			if rate := snapshotMultiplier(current); rate != nil {
				totals.theoretical += *delta * *rate
				totals.theoreticalPairs++
			}
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
	// 逐对倍率累加出的理论值优先于「单一倍率×整窗原价」，
	// 否则窗口内的倍率调整会整段计入偏差。
	if totals.theoreticalPairs > 0 && totals.theoreticalPairs == totals.reportedPairs {
		theoretical := totals.theoretical
		audit.TheoreticalCost = &theoretical
		audit.theoreticalPerPair = true
	}

	if err := s.applyLocalPricing(&audit, upstreamID); err != nil {
		audit.Status = "unavailable"
		audit.Reason = "pricing_query_failed"
		return audit, nil
	}
	evaluateBillingAudit(&audit, multiplier)
	return audit, nil
}

// evaluateBillingAudit 执行双轨判定，是即时窗口与区间聚合共用的收尾。
//
// 轨道一（价目核对）：本地 ListCost 与上游自报原价的差距。上游自报明显高于公共
// 行情说明它挂牌价虚标——余额按虚标价扣，同样是真金白银的损失，故超阈值告警。
// 仅在两侧都有非零估算时才判，否则无意义。
//
// 轨道二（计费核对）：ActualCost 与「基准×倍率」的差距。基准优先取上游自报原价
// ——用上游自己的挂牌价算，结论不受本地价表漂移污染，这才是真正查倍率有没有被
// 正确应用。上游不提供原价时（如 newAPI）降级用本地 ListCost，BillingBasis 标为
// "local"：此时偏差可能只反映本地价表低估，故不升级为 warning，仅作提示。
//
// 关键约束：本地价目不完整（LocalPricingReason 非空）只影响轨道一，绝不能阻塞
// 轨道二——上游自报原价与实际扣费都在手上时，账目对不对是能算清的。
func evaluateBillingAudit(audit *BillingAudit, multiplier *float64) {
	if multiplier == nil {
		audit.Status = "unavailable"
		audit.Reason = "multiplier_unavailable"
		return
	}

	// 轨道一：价目核对。仅在两侧都有**非零**估算时才有意义 ——
	// 本地为 0 通常是窗口内无请求或模型别名查不到价，不是上游虚标；
	// 拿 0 去比会把任何上游消费都判成虚标。
	catalogMismatch := false
	if audit.ListCost != nil && audit.ReportedListCost != nil &&
		*audit.ListCost > 1e-9 && *audit.ReportedListCost > 1e-9 {
		catalogDeviation := *audit.ReportedListCost - *audit.ListCost
		audit.CatalogDeviation = &catalogDeviation
		reference := math.Max(*audit.ListCost, *audit.ReportedListCost)
		rate := catalogDeviation / reference
		audit.CatalogRate = &rate
		// 只在上游自报「高于」本地估算时告警；低于说明上游给了折扣价，不是问题。
		catalogMismatch = rate > billingCatalogTolerance
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
		// 两个基准都没有才真的无从比对。优先报本地价目的具体降级原因，
		// 它比笼统的 pricing_catalog_unavailable 更能指向要修的东西。
		audit.Status = "unavailable"
		audit.Reason = audit.LocalPricingReason
		if audit.Reason == "" {
			audit.Reason = "pricing_catalog_unavailable"
		}
		return
	}

	theoretical := *basis * *multiplier
	// 区间聚合已用逐对倍率算好理论值时以它为准：倍率在窗口内变动过的话，
	// 单一倍率×整窗原价会把调整时点的落差整段计入偏差。
	if audit.TheoreticalCost != nil {
		theoretical = *audit.TheoreticalCost
	} else {
		audit.TheoreticalCost = &theoretical
	}
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
	// 两种情况不升级为 warning，只保留说明：
	//   basis=local —— 偏差可能只反映本地价表低估，不足以指控上游多收；
	//   倍率变动过且理论值不是逐对算出的 —— 偏差来自倍率调整时点，不是超收。
	if deviation > tolerance {
		trustworthy := audit.BillingBasis == "reported" && !perPairUnavailableWithChange(audit)
		if trustworthy {
			audit.Status = "warning"
			audit.Reason = "actual_cost_exceeded"
			return
		}
		audit.Status = "ok"
		if audit.BillingBasis == "reported" {
			audit.Reason = "multiplier_changed"
		} else {
			audit.Reason = "actual_cost_exceeded_local_basis"
		}
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

// perPairUnavailableWithChange 报告「倍率在窗口内变动过，但理论值只能用单一倍率
// 估算」的情形——此时聚合比对不成立，偏差不可用于判定超收。
func perPairUnavailableWithChange(audit *BillingAudit) bool {
	return audit.MultiplierChanged && !audit.theoreticalPerPair
}
