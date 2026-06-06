package health

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recAlerter 记录收到的翻转事件，供「仅翻转触发」断言用。
type recAlerter struct {
	mu  sync.Mutex
	evs []AlertEvent
}

func (r *recAlerter) Notify(ev AlertEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}

func (r *recAlerter) snapshot() []AlertEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AlertEvent(nil), r.evs...)
}

// 仅在状态翻转时触发，且 HalfOpen 中间态不单发。
func TestAlertOnlyOnFlip(t *testing.T) {
	rec := &recAlerter{}
	m := New(3, 30*time.Millisecond)
	m.SetAlerter(rec)
	const id = int64(1)

	// 前两次失败未达阈值 → 不翻转、不告警
	m.Report(id, "", false, 0)
	m.Report(id, "", false, 0)
	// 第三次失败 → Closed→Open，发「熔断」
	m.Report(id, "", false, 0)
	// 冷却后转 HalfOpen 再失败 → HalfOpen→Open，不单独发
	time.Sleep(40 * time.Millisecond)
	m.IsAvailable(id, "") // 触发 → HalfOpen
	m.Report(id, "", false, 0)
	// 冷却后 HalfOpen + 成功 → 恢复，发「恢复」
	time.Sleep(40 * time.Millisecond)
	m.IsAvailable(id, "") // → HalfOpen
	m.Report(id, "", true, 50)

	if !waitFor(func() bool { return len(rec.snapshot()) == 2 }, 2*time.Second) {
		t.Fatalf("应收到 2 条翻转告警，实际 %d", len(rec.snapshot()))
	}
	evs := rec.snapshot()
	if evs[0].FromState != "CLOSED" || evs[0].ToState != "OPEN" {
		t.Fatalf("第一条应 CLOSED→OPEN，实际 %+v", evs[0])
	}
	if evs[1].ToState != "CLOSED" {
		t.Fatalf("第二条应恢复→CLOSED，实际 %+v", evs[1])
	}
}

// 普通失败/成功不翻转就不告警。
func TestNoAlertWithoutFlip(t *testing.T) {
	rec := &recAlerter{}
	m := New(100, time.Second) // 高阈值，不会熔断
	m.SetAlerter(rec)
	for i := 0; i < 10; i++ {
		m.Report(2, "", false, 0)
		m.Report(2, "", true, 10)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("未翻转不应告警，实际发了 %d 条", n)
	}
}

// 去抖：同键窗口内只发一次；不同键各自独立。
func TestWebhookDebounce(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()

	a := NewWebhookAlerter(
		func() string { return srv.URL },
		func() time.Duration { return time.Hour }, // 大窗口，确保第二次被抖掉
		nil,
	)
	ev := AlertEvent{UpstreamID: 1, Model: "", FromState: "CLOSED", ToState: "OPEN", TS: time.Now().Unix()}
	a.Notify(ev)
	a.Notify(ev) // 同键，窗口内 → 被丢弃
	a.Notify(AlertEvent{UpstreamID: 2, Model: "", FromState: "CLOSED", ToState: "OPEN"}) // 不同键 → 放行

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("同键去抖后应共 2 次(键1一次+键2一次)，实际 %d", got)
	}
}

// 空 URL → 整体关闭，不发。
func TestWebhookEmptyDisabled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	a := NewWebhookAlerter(func() string { return "" }, func() time.Duration { return time.Second }, nil)
	a.Notify(AlertEvent{UpstreamID: 1, FromState: "CLOSED", ToState: "OPEN"})
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("空 URL 应关闭告警，实际发了 %d 次", got)
	}
}

// 载荷断言 + 异步不阻塞：webhook 慢，Report 必须立即返回。
func TestWebhookPayloadAndAsync(t *testing.T) {
	got := make(chan alertPayload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // 故意慢
		var p alertPayload
		json.NewDecoder(r.Body).Decode(&p)
		got <- p
	}))
	defer srv.Close()

	a := NewWebhookAlerter(
		func() string { return srv.URL },
		func() time.Duration { return time.Minute },
		func(id int64) string { return "up-" + itoa(id) }, // 名字解析
	)
	m := New(1, time.Minute) // 一次失败即熔断
	m.SetAlerter(a)

	start := time.Now()
	m.Report(7, "gpt-x", false, 0) // Closed→Open
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Report 不应被慢 webhook 阻塞，耗时 %v", elapsed)
	}

	select {
	case p := <-got:
		if p.UpstreamID != 7 || p.UpstreamName != "up-7" || p.Model != "gpt-x" {
			t.Fatalf("载荷字段不符：%+v", p)
		}
		if p.FromState != "CLOSED" || p.ToState != "OPEN" || p.Fails != 1 {
			t.Fatalf("载荷状态不符：%+v", p)
		}
		if p.TS == 0 {
			t.Fatal("载荷 ts 应为 unix 秒")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超时未收到 webhook")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
