package monitor

import (
	"sync"
	"time"
)

// 栅栏状态档位（与前端 Fence 对应）：0 无数据 1 正常 2 降级 3 故障。
const (
	statNoData   = 0
	statOK       = 1
	statDegraded = 2
	statDown     = 3
	trendCap     = 60 // 趋势环形缓冲容量
)

// Point 一次探测采样点。
type Point struct {
	TS     int64 `json:"ts"`
	Status int   `json:"status"`
	LatMs  int64 `json:"lat_ms"`
}

// stats 单个监控项的探测统计（仅内存，重启清零）。
type stats struct {
	reqs, fails int64
	lastLatMs   int64
	totLatency  int64 // 累计延迟(ms)，仅成功
	trend       []Point
}

// Snapshot 看板视图。
type Snapshot struct {
	State    string  `json:"state"`     // OK/DEGRADED/DOWN/NODATA
	Reqs     int64   `json:"reqs"`      // 探测总次数
	SuccRate float64 `json:"succ_rate"` // 成功率 0..1
	LastMs   int64   `json:"last_ms"`   // 最近一次延迟
	AvgMs    int64   `json:"avg_ms"`    // 成功平均延迟
	Trend    []Point `json:"trend"`
	LastTS   int64   `json:"last_ts"` // 最后探测时间(unix秒)，0=未探测
}

// Manager 监控统计管理：探测器调 Record，看板读 Snapshot。
type Manager struct {
	mu sync.Mutex
	m  map[int64]*stats
}

func New() *Manager { return &Manager{m: make(map[int64]*stats)} }

func (mgr *Manager) get(id int64) *stats {
	s := mgr.m[id]
	if s == nil {
		s = &stats{}
		mgr.m[id] = s
	}
	return s
}

// Record 记录一次探测结果。status: 1正常 2降级(如429) 3故障。
func (mgr *Manager) Record(id int64, status int, latMs int64) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	s := mgr.get(id)
	s.reqs++
	s.lastLatMs = latMs
	if status == statOK {
		s.totLatency += latMs
	} else {
		s.fails++
	}
	s.trend = append(s.trend, Point{TS: time.Now().Unix(), Status: status, LatMs: latMs})
	if len(s.trend) > trendCap {
		s.trend = s.trend[len(s.trend)-trendCap:]
	}
}

// Forget 删除监控项后清理其统计。
func (mgr *Manager) Forget(id int64) {
	mgr.mu.Lock()
	delete(mgr.m, id)
	mgr.mu.Unlock()
}

func (mgr *Manager) Snapshot(id int64) Snapshot {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	s := mgr.get(id)
	sn := Snapshot{State: "NODATA", Reqs: s.reqs, LastMs: s.lastLatMs,
		Trend: append([]Point(nil), s.trend...)}
	if succ := s.reqs - s.fails; succ > 0 {
		sn.AvgMs = s.totLatency / succ
	}
	if s.reqs > 0 {
		sn.SuccRate = float64(s.reqs-s.fails) / float64(s.reqs)
		last := s.trend[len(s.trend)-1]
		sn.LastTS = last.TS
		switch last.Status {
		case statDown:
			sn.State = "DOWN"
		case statDegraded:
			sn.State = "DEGRADED"
		default:
			sn.State = "OK"
		}
	}
	return sn
}
