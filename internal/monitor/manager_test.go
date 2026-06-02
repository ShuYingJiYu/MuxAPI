package monitor

import "testing"

func TestSnapshotStates(t *testing.T) {
	m := New()

	// 无数据
	if sn := m.Snapshot(1); sn.State != "NODATA" || sn.Reqs != 0 {
		t.Fatalf("空监控项应为 NODATA/0，得到 %+v", sn)
	}

	// 正常：两次成功
	m.Record(1, statOK, 100)
	m.Record(1, statOK, 200)
	sn := m.Snapshot(1)
	if sn.State != "OK" || sn.SuccRate != 1 || sn.AvgMs != 150 || sn.LastMs != 200 {
		t.Fatalf("两次成功应 OK/1.0/avg150/last200，得到 %+v", sn)
	}

	// 降级：再来一次 429（最近一次决定 State）
	m.Record(1, statDegraded, 50)
	if sn := m.Snapshot(1); sn.State != "DEGRADED" || sn.Reqs != 3 {
		t.Fatalf("最近降级应 DEGRADED/3，得到 %+v", sn)
	}

	// 故障：最近一次失败
	m.Record(1, statDown, 0)
	sn = m.Snapshot(1)
	if sn.State != "DOWN" {
		t.Fatalf("最近故障应 DOWN，得到 %+v", sn)
	}
	if sn.SuccRate != 0.5 { // 4次里2次成功
		t.Fatalf("成功率应 0.5，得到 %v", sn.SuccRate)
	}

	// 清理
	m.Forget(1)
	if sn := m.Snapshot(1); sn.State != "NODATA" {
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
