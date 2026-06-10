package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestStoreCRUD(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 空库
	if list, _ := st.ListEnabledByGroup(1); len(list) != 0 {
		t.Fatal("空库应无上游")
	}

	// 建分组 + 全局上游（B 停用）
	g1, _ := st.CreateGroup("g1", "first")
	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://a", APIKey: "ka", Enabled: true})
	st.Create(&upstream.Upstream{Name: "B", BaseURL: "http://b", APIKey: "kb", Enabled: false})
	ups, _ := st.List()
	if len(ups) != 2 {
		t.Fatalf("全局池应有 2 个上游，实际 %d", len(ups))
	}
	idA, idB := ups[0].ID, ups[1].ID

	// 加入 g1：A 组内优先级 1，B 优先级 2
	st.AddMember(g1, idA, 1, 1)
	st.AddMember(g1, idB, 2, 1)

	// ListEnabledByGroup 只返回启用成员（A），且带组内 priority
	list, err := st.ListEnabledByGroup(g1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "A" || list[0].Priority != 1 {
		t.Fatalf("应只返回启用的 A(组内prio=1)，实际 %+v", list)
	}
	if list[0].APIKey != "ka" || list[0].BaseURL != "http://a" {
		t.Fatalf("字段读取错误 %+v", list[0])
	}

	// M:N：同一上游 A 加入 g2，组内优先级不同(10)
	g2, _ := st.CreateGroup("g2", "")
	st.AddMember(g2, idA, 10, 5)
	l2, _ := st.ListEnabledByGroup(g2)
	if len(l2) != 1 || l2[0].Priority != 10 || l2[0].Weight != 5 {
		t.Fatalf("A 在 g2 应有独立组内策略 prio=10 weight=5，实际 %+v", l2)
	}
	// g1 里 A 的策略不受影响
	l1, _ := st.ListEnabledByGroup(g1)
	if l1[0].Priority != 1 {
		t.Fatalf("g1 中 A 的组内优先级应仍为 1，实际 %d", l1[0].Priority)
	}

	// 系统生成密钥 + 启停过滤
	key, _ := st.CreateKey("k1", g1)
	if gid, ok := st.GroupByKey(key); !ok || gid != g1 {
		t.Fatalf("启用 key 应解析到 g1")
	}
	ks, _ := st.ListKeys(g1)
	st.SetKeyEnabled(ks[0].ID, false)
	if _, ok := st.GroupByKey(key); ok {
		t.Fatal("停用 key 不应再解析到分组")
	}
}

func TestMemberEnabled(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	g1, _ := st.CreateGroup("g1", "")
	g2, _ := st.CreateGroup("g2", "")
	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://a", APIKey: "ka", Enabled: true})
	ups, _ := st.List()
	idA := ups[0].ID

	// A 同时加入 g1、g2，默认组内启用
	st.AddMember(g1, idA, 10, 1)
	st.AddMember(g2, idA, 10, 1)
	if list, _ := st.ListEnabledByGroup(g1); len(list) != 1 {
		t.Fatalf("默认组内启用，g1 应调度到 A，实际 %d", len(list))
	}

	// 仅在 g1 内停用 A
	if err := st.SetMemberEnabled(g1, idA, false); err != nil {
		t.Fatal(err)
	}
	// g1 调度不再返回 A
	if list, _ := st.ListEnabledByGroup(g1); len(list) != 0 {
		t.Fatalf("g1 内停用后不应调度到 A，实际 %d", len(list))
	}
	// 但成员列表仍可见，GroupEnabled=false（保留 priority/weight）
	ms, _ := st.ListGroupMembers(g1)
	if len(ms) != 1 || ms[0].GroupEnabled || ms[0].Priority != 10 {
		t.Fatalf("g1 成员应仍可见且 GroupEnabled=false、prio 保留，实际 %+v", ms)
	}
	// 全局开关不受影响
	if !ms[0].Enabled {
		t.Fatal("组内停用不应改动全局 enabled")
	}
	// 其他分组 g2 不受影响，照常调度
	if list, _ := st.ListEnabledByGroup(g2); len(list) != 1 {
		t.Fatalf("g2 不应受 g1 组内停用影响，实际 %d", len(list))
	}

	// 恢复 g1 内启用
	st.SetMemberEnabled(g1, idA, true)
	if list, _ := st.ListEnabledByGroup(g1); len(list) != 1 {
		t.Fatalf("恢复后 g1 应重新调度到 A，实际 %d", len(list))
	}
}

func TestMonitorConfigFields(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://a", APIKey: "k", Enabled: true})
	ups, _ := st.List()
	uid := ups[0].ID

	// 建带全部可配字段的监控项
	id, err := st.CreateMonitor(&Monitor{
		UpstreamID: uid, Model: "gpt-5.5", Enabled: true,
		Stream: true, ProbeText: "ping", MaxTokens: 8, IntervalSec: 120, Path: "/v1/messages",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMonitor(id)
	if !got.Stream || got.ProbeText != "ping" || got.MaxTokens != 8 || got.IntervalSec != 120 || got.Path != "/v1/messages" {
		t.Fatalf("可配字段未持久化: %+v", got)
	}

	// 更新这些字段
	got.Stream = false
	got.IntervalSec = 0
	got.Path = ""
	if err := st.UpdateMonitor(got); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.GetMonitor(id)
	if got2.Stream || got2.IntervalSec != 0 || got2.Path != "" {
		t.Fatalf("更新后字段不符(空/0=继承全局): %+v", got2)
	}
}

func TestReorderMonitors(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://a", APIKey: "k", Enabled: true})
	uid := func() int64 { ups, _ := st.List(); return ups[0].ID }()

	// 建 3 个监控项，默认 sort=0 → 按 id 升序
	var ids []int64
	for _, m := range []string{"m1", "m2", "m3"} {
		id, _ := st.CreateMonitor(&Monitor{UpstreamID: uid, Model: m, Enabled: true})
		ids = append(ids, id)
	}
	list, _ := st.ListMonitors(false)
	if len(list) != 3 || list[0].ID != ids[0] || list[2].ID != ids[2] {
		t.Fatalf("默认应按 id 升序，得到 %v", modelOrder(list))
	}

	// 倒序保存
	if err := st.ReorderMonitors([]int64{ids[2], ids[1], ids[0]}); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListMonitors(false)
	if list[0].ID != ids[2] || list[1].ID != ids[1] || list[2].ID != ids[0] {
		t.Fatalf("应按保存顺序倒序，得到 %v", modelOrder(list))
	}
}

func modelOrder(ms []*Monitor) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Model
	}
	return out
}

func TestPruneLogs(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 0; i < 50; i++ {
		st.Log(1, 1, "gpt-test", "/v1/messages", "k1", 0, 200, 10, "")
	}

	// keep<=0：关闭清理，不删任何行
	if n, _ := st.PruneLogs(0); n != 0 {
		t.Fatalf("keep=0 应不删，实际删 %d", n)
	}
	if got, _ := st.ListLogs(1000); len(got) != 50 {
		t.Fatalf("应仍有 50 条，实际 %d", len(got))
	}

	// keep>总数：不删
	if n, _ := st.PruneLogs(100); n != 0 {
		t.Fatalf("keep>总数应不删，实际删 %d", n)
	}

	// 保留最新 20 条：删 30
	n, err := st.PruneLogs(20)
	if err != nil {
		t.Fatal(err)
	}
	if n != 30 {
		t.Fatalf("应删 30 条，实际 %d", n)
	}
	got, _ := st.ListLogs(1000)
	if len(got) != 20 {
		t.Fatalf("应剩 20 条，实际 %d", len(got))
	}
	// 保留的应是最新的（id 最大那批），ListLogs 倒序，首条 id 应为 50
	if got[0].ID != 50 {
		t.Fatalf("保留的应是最新批次(首条 id=50)，实际 id=%d", got[0].ID)
	}
}

// 直接按指定时刻插探测行（绕过 InsertProbe 的 now），便于构造跨小时桶。
func insertProbeAt(t *testing.T, st *Store, monitorID int64, status int, latMs, ts int64) {
	t.Helper()
	if _, err := st.db.Exec(`INSERT INTO probe_results(monitor_id,status,latency_ms,created_at) VALUES(?,?,?,?)`,
		monitorID, status, latMs, ts); err != nil {
		t.Fatalf("插探测行失败: %v", err)
	}
}

func TestMonitorHourlyTrendAndRecent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	curHour := now.Truncate(time.Hour).Unix()
	prevHour := curHour - 3600

	// 上一小时：4 次探测，3 成功 1 故障 → succ_rate=.75 → status=3(<.80)
	insertProbeAt(t, st, 1, 1, 100, prevHour+10)
	insertProbeAt(t, st, 1, 1, 200, prevHour+20)
	insertProbeAt(t, st, 1, 1, 300, prevHour+30)
	insertProbeAt(t, st, 1, 3, 0, prevHour+40)
	// 当前小时：2 次全成功 → succ_rate=1 → status=1
	insertProbeAt(t, st, 1, 1, 150, curHour+10)
	insertProbeAt(t, st, 1, 1, 250, curHour+20)

	trend := st.MonitorHourlyTrend(1)
	if len(trend) != 24 {
		t.Fatalf("趋势应 24 桶，实际 %d", len(trend))
	}
	// 最后一桶=当前小时，倒数第二=上一小时
	cur, prev := trend[23], trend[22]
	if cur.Ts != curHour || prev.Ts != prevHour {
		t.Fatalf("末两桶时刻应为 当前/上一小时，得到 %d/%d", cur.Ts, prev.Ts)
	}
	if cur.Total != 2 || cur.Succ != 2 || cur.SuccRate != 1 || cur.Status != 1 {
		t.Fatalf("当前小时应 2次/2成功/100%%/status1，得到 %+v", cur)
	}
	if prev.Total != 4 || prev.Succ != 3 || prev.SuccRate != 0.75 || prev.Status != 3 {
		t.Fatalf("上一小时应 4次/3成功/75%%/status3，得到 %+v", prev)
	}
	// 更早的桶应为空（status0）
	if trend[0].Total != 0 || trend[0].Status != 0 {
		t.Fatalf("最早桶应空，得到 %+v", trend[0])
	}

	// 近 24h 汇总：6 次，5 成功；成功均延迟 = (100+200+300+150+250)/5 = 200
	reqs, succ, avgMs, lastMs, lastTS, lastStatus := st.MonitorRecent(1)
	if reqs != 6 || succ != 5 {
		t.Fatalf("应 6次5成功，得到 %d/%d", reqs, succ)
	}
	if avgMs != 200 {
		t.Fatalf("成功均延迟应 200，得到 %d", avgMs)
	}
	if lastStatus != 1 || lastTS != curHour+20 || lastMs != 250 {
		t.Fatalf("最近一次应 成功/curHour+20/250ms，得到 status=%d ts=%d ms=%d", lastStatus, lastTS, lastMs)
	}

	// 空监控项：全零
	if reqs, _, _, _, lastTS, _ := st.MonitorRecent(999); reqs != 0 || lastTS != 0 {
		t.Fatalf("空监控项应全零，得到 reqs=%d lastTS=%d", reqs, lastTS)
	}
}

func TestPruneProbes(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().Unix()
	insertProbeAt(t, st, 1, 1, 10, now-1*3600)   // 1h 前，保留
	insertProbeAt(t, st, 1, 1, 10, now-47*3600)  // 47h 前，保留
	insertProbeAt(t, st, 1, 1, 10, now-49*3600)  // 49h 前，删
	insertProbeAt(t, st, 1, 1, 10, now-100*3600) // 100h 前，删

	// keepHours<=0：不删
	if n, _ := st.PruneProbes(0); n != 0 {
		t.Fatalf("keep=0 应不删，实际 %d", n)
	}
	n, err := st.PruneProbes(48)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应删 2 条(>48h)，实际 %d", n)
	}

	// ForgetProbes 清空该监控项
	if err := st.ForgetProbes(1); err != nil {
		t.Fatal(err)
	}
	if reqs, _, _, _, _, _ := st.MonitorRecent(1); reqs != 0 {
		t.Fatalf("ForgetProbes 后应无记录，实际 %d", reqs)
	}
}

// countRows 小工具：直读某表行数，校验级联删除是否干净。
func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestDeleteUpstreamCascade 验证 Delete 事务化后：删上游会一并清掉中间表与监控项，
// 提交成功则三表均无残留（事务原子性回归——任一步失败整体回滚，不留半删）。
func TestDeleteUpstreamCascade(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	g1, _ := st.CreateGroup("g1", "")
	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://a", APIKey: "k", Enabled: true})
	uid := func() int64 { ups, _ := st.List(); return ups[0].ID }()
	st.AddMember(g1, uid, 1, 1)
	st.CreateMonitor(&Monitor{UpstreamID: uid, Model: "m1", Enabled: true})
	st.CreateMonitor(&Monitor{UpstreamID: uid, Model: "m2", Enabled: true})

	// 删前：三表均有该上游相关行
	if countRows(t, st, "upstreams") != 1 || countRows(t, st, "group_upstreams") != 1 || countRows(t, st, "monitors") != 2 {
		t.Fatal("前置数据未就绪")
	}

	if err := st.Delete(uid); err != nil {
		t.Fatal(err)
	}
	// 删后：父子表全干净，无半删
	if n := countRows(t, st, "upstreams"); n != 0 {
		t.Fatalf("upstreams 应清空，残留 %d", n)
	}
	if n := countRows(t, st, "group_upstreams"); n != 0 {
		t.Fatalf("group_upstreams 应清空，残留 %d", n)
	}
	if n := countRows(t, st, "monitors"); n != 0 {
		t.Fatalf("monitors 应清空，残留 %d", n)
	}
}

// TestDeleteGroupCascade 验证 DeleteGroup 事务化后：删分组会一并清掉成员关系与接入密钥，
// 提交成功则 groups/group_upstreams/access_keys 三表均无该组残留。
func TestDeleteGroupCascade(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	g1, _ := st.CreateGroup("g1", "")
	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://a", APIKey: "k", Enabled: true})
	uid := func() int64 { ups, _ := st.List(); return ups[0].ID }()
	st.AddMember(g1, uid, 1, 1)
	st.CreateKey("k1", g1)

	if countRows(t, st, "groups") != 1 || countRows(t, st, "group_upstreams") != 1 || countRows(t, st, "access_keys") != 1 {
		t.Fatal("前置数据未就绪")
	}

	if err := st.DeleteGroup(g1); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "groups"); n != 0 {
		t.Fatalf("groups 应清空，残留 %d", n)
	}
	if n := countRows(t, st, "group_upstreams"); n != 0 {
		t.Fatalf("group_upstreams 应清空，残留 %d", n)
	}
	if n := countRows(t, st, "access_keys"); n != 0 {
		t.Fatalf("access_keys 应清空，残留 %d", n)
	}
	// 上游全局池不受删组影响
	if n := countRows(t, st, "upstreams"); n != 1 {
		t.Fatalf("删组不应动全局上游，实际 %d", n)
	}
}
