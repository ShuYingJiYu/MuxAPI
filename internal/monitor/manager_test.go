package monitor

import (
	"testing"

	"github.com/mirainya/muxapi/internal/store"
)

// newTestManager 用内存库建一个监控项，返回其 Manager 与监控项 ID。
func newTestManager(t *testing.T) (*Manager, int64) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.CreateMonitor(&store.Monitor{UpstreamID: 1, Model: "m"})
	if err != nil {
		t.Fatalf("建监控项失败: %v", err)
	}
	return New(st), id
}

func TestSnapshotStates(t *testing.T) {
	m, id := newTestManager(t)

	// 无数据
	if sn := m.Snapshot(id); sn.State != "NODATA" || sn.Reqs != 0 {
		t.Fatalf("空监控项应为 NODATA/0，得到 %+v", sn)
	}

	// 正常：两次成功（同一小时桶内）
	m.Record(id, statOK, 100)
	m.Record(id, statOK, 200)
	sn := m.Snapshot(id)
	if sn.State != "OK" || sn.SuccRate != 1 || sn.AvgMs != 150 || sn.LastMs != 200 {
		t.Fatalf("两次成功应 OK/1.0/avg150/last200，得到 %+v", sn)
	}

	// 降级：再来一次 429（最近一次决定 State）
	m.Record(id, statDegraded, 50)
	if sn := m.Snapshot(id); sn.State != "DEGRADED" || sn.Reqs != 3 {
		t.Fatalf("最近降级应 DEGRADED/3，得到 %+v", sn)
	}

	// 故障：最近一次失败
	m.Record(id, statDown, 0)
	sn = m.Snapshot(id)
	if sn.State != "DOWN" {
		t.Fatalf("最近故障应 DOWN，得到 %+v", sn)
	}
	if sn.SuccRate != 0.5 { // 4次里2次成功
		t.Fatalf("成功率应 0.5，得到 %v", sn.SuccRate)
	}

	// 趋势恒为 24 小时桶
	if len(sn.Trend) != 24 {
		t.Fatalf("趋势应为 24 小时桶，得到 %d", len(sn.Trend))
	}

	// 清理
	m.Forget(id)
	if sn := m.Snapshot(id); sn.State != "NODATA" {
		t.Fatalf("Forget 后应回 NODATA，得到 %+v", sn)
	}
}

func TestClassify(t *testing.T) {
	cases := map[int]int{200: statOK, 204: statOK, 429: statDegraded, 401: statDown, 500: statDown}
	for code, want := range cases {
		if got := classify(code); got != want {
			t.Errorf("classify(%d)=%d, 期望 %d", code, got, want)
		}
	}
}
