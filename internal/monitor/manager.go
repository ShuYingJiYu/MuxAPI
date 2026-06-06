package monitor

import (
	"github.com/mirainya/muxapi/internal/store"
)

// 栅栏状态档位（与前端 Fence 对应）：0 无数据 1 正常 2 降级 3 故障。
const (
	statNoData   = 0
	statOK       = 1
	statDegraded = 2
	statDown     = 3
)

// Snapshot 看板视图。探测统计已落库按小时分桶（见 store），本类型仅做组装。
type Snapshot struct {
	State    string           `json:"state"`     // OK/DEGRADED/DOWN/NODATA
	Reqs     int64            `json:"reqs"`      // 近 24h 探测次数
	SuccRate float64          `json:"succ_rate"` // 近 24h 成功率 0..1
	LastMs   int64            `json:"last_ms"`   // 最近一次延迟
	AvgMs    int64            `json:"avg_ms"`    // 近 24h 成功平均延迟
	Trend    []store.HourPoint `json:"trend"`    // 近 24h 按小时分桶的成功率栅栏
	LastTS   int64            `json:"last_ts"`   // 最后探测时间(unix秒)，0=未探测
}

// Manager 监控统计管理：探测器调 Record 落库，看板读 Snapshot 按小时聚合。
// 统计持久化在 store，重启不丢，与分组的请求成功率同一套口径。
type Manager struct {
	store *store.Store
}

func New(st *store.Store) *Manager { return &Manager{store: st} }

// Record 记录一次探测结果落库。status: 1正常 2降级(如429) 3故障。
func (mgr *Manager) Record(id int64, status int, latMs int64) {
	_ = mgr.store.InsertProbe(id, status, latMs)
}

// Forget 删除监控项后清理其探测记录。
func (mgr *Manager) Forget(id int64) {
	_ = mgr.store.ForgetProbes(id)
}

// Snapshot 组装看板视图：近 24h 成功率/均延迟 + 最近一次状态 + 24 小时栅栏。
func (mgr *Manager) Snapshot(id int64) Snapshot {
	reqs, succ, avgMs, lastMs, lastTS, lastStatus := mgr.store.MonitorRecent(id)
	sn := Snapshot{
		State: "NODATA", Reqs: int64(reqs), AvgMs: avgMs, LastMs: lastMs, LastTS: lastTS,
		Trend: mgr.store.MonitorHourlyTrend(id),
	}
	if reqs > 0 {
		sn.SuccRate = float64(succ) / float64(reqs)
	}
	// State 由最近一次探测决定（与改造前行为一致）；无任何探测则 NODATA。
	if lastTS > 0 {
		switch lastStatus {
		case statDown:
			sn.State = "DOWN"
		case statDegraded:
			sn.State = "DEGRADED"
		case statOK:
			sn.State = "OK"
		}
	}
	return sn
}
