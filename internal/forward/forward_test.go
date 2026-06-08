package forward

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

// noopLogger 测试用空日志器
type noopLogger struct{}

func (noopLogger) Log(groupID, upstreamID int64, model, endpoint, keyName string, retries, status int, latencyMs int64) {
}

// 验证 SSE 流式逐行透传不丢事件、不破坏格式。
func TestForwardSSEStreaming(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for _, ev := range []string{
			"event: message_start",
			`data: {"type":"message_start"}`,
			"",
			"event: content_block_delta",
			`data: {"text":"hi"}`,
			"",
		} {
			io.WriteString(w, ev+"\n")
			fl.Flush()
		}
	}))
	defer upSrv.Close()

	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: upSrv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	res := rec.Result()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("应透传 SSE Content-Type，实际 %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	for _, want := range []string{"message_start", `"text":"hi"`, "content_block_delta"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("SSE 应含 %q，实际:\n%s", want, body)
		}
	}
}

// 验证首个上游流式失败(5xx)时回切到下一个上游并成功流式透传。
func TestForwardSSEFailover(t *testing.T) {
	var aHits, bHits int32
	// 上游 A：优先级高，但 503 失败
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.WriteHeader(503)
	}))
	defer srvA.Close()
	// 上游 B：备用，正常吐 SSE
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"text":"from-B"}`+"\n")
		fl.Flush()
	}))
	defer srvB.Close()

	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", BaseURL: srvA.URL, APIKey: "k", Priority: 10, Enabled: true},
		{ID: 2, Name: "B", BaseURL: srvB.URL, APIKey: "k", Priority: 20, Enabled: true},
	}
	hm := health.New(1, time.Hour) // 阈值1：A一失败立即熔断，下次Pick避开
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("回切后应 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "from-B") {
		t.Fatalf("应回切到 B 并透传其 SSE，实际:\n%s", body)
	}
	if atomic.LoadInt32(&aHits) != 1 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("A 应被试 1 次、B 1 次，实际 A=%d B=%d", aHits, bHits)
	}
	if !hm.IsAvailable(2, "") || hm.IsAvailable(1, "") {
		t.Fatal("失败后 A 应熔断、B 应可用")
	}
}

// 验证按失败原因区分熔断范围：5xx 仅熔断 (上游,模型)，401 熔断整上游。
func TestForwardFailScope(t *testing.T) {
	// 503 上游：触发模型级熔断
	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv503.Close()
	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: srv503.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(1, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 0)

	body := []byte(`{"model":"gpt-x","stream":false}`)
	fwd.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))), body, 0, "")
	if hm.IsAvailable(1, "gpt-x") {
		t.Fatal("503 应熔断 (A, gpt-x)")
	}
	if !hm.IsAvailable(1, "gpt-y") {
		t.Fatal("503 只熔断 gpt-x，gpt-y 不应受影响 ← 模型级隔离")
	}
	if !hm.IsAvailable(1, "") {
		t.Fatal("503 是模型级，上游级不应熔断")
	}

	// 401 上游：触发上游级熔断（所有模型连坐）
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv401.Close()
	ups2 := []*upstream.Upstream{{ID: 2, Name: "C", BaseURL: srv401.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm2 := health.New(1, time.Hour)
	fwd2 := New(scheduler.New(func(int64) []*upstream.Upstream { return ups2 }, hm2), hm2, noopLogger{}, 0)
	body2 := []byte(`{"model":"gpt-x"}`)
	fwd2.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body2))), body2, 0, "")
	if hm2.IsAvailable(2, "anything") {
		t.Fatal("401 应熔断整个上游，所有模型连坐")
	}
}

// 验证 SSE 是逐块 flush 透传，而非全缓冲后一次性返回。
func TestForwardSSEIncrementalFlush(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"chunk":1}`+"\n")
		fl.Flush()
		<-release // 阻塞，直到测试确认已收到首块才吐第二块
		io.WriteString(w, `data: {"chunk":2}`+"\n")
		fl.Flush()
	}))
	defer srv.Close()

	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 3)

	pr, pw := io.Pipe()
	rec := &flushRecorder{header: http.Header{}, w: pw, flushed: make(chan struct{}, 8)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	go fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	br := bufio.NewReader(pr)
	line1, _ := br.ReadString('\n')
	if !strings.Contains(line1, `"chunk":1`) {
		t.Fatalf("首块应先到达，实际 %q", line1)
	}
	select {
	case <-rec.flushed: // 确认首块是被 flush 出来的，不是缓冲
	case <-time.After(time.Second):
		t.Fatal("首块应触发 flush")
	}
	close(release) // 放行第二块
	line2, _ := br.ReadString('\n')
	if !strings.Contains(line2, `"chunk":2`) {
		t.Fatalf("第二块应随后到达，实际 %q", line2)
	}
	pr.Close()
}

// 回归：默认熔断阈值(>1)下，单次请求内首个上游失败也应立即切到下一个。
// 旧实现依赖「失败即熔断」才能换上游，阈值3时一次失败不熔断 → Pick 又选回 A → 误判 502。
func TestForwardFailoverWithHighThreshold(t *testing.T) {
	var aHits, bHits int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.WriteHeader(503)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.WriteHeader(200)
		io.WriteString(w, `{"text":"from-B"}`)
	}))
	defer srvB.Close()

	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", BaseURL: srvA.URL, APIKey: "k", Priority: 10, Enabled: true},
		{ID: 2, Name: "B", BaseURL: srvB.URL, APIKey: "k", Priority: 20, Enabled: true},
	}
	hm := health.New(3, time.Hour) // 默认阈值3：A 一次失败不熔断，仍须切到 B
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	fwd.Forward(rec, req, []byte(`{}`), 0, "")

	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("阈值3下首个上游失败也应切到 B 返回 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "from-B") {
		t.Fatalf("应切到 B，实际:\n%s", body)
	}
	if atomic.LoadInt32(&aHits) != 1 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("A 应被试 1 次、B 1 次，实际 A=%d B=%d", aHits, bHits)
	}
}

// 回归(生产事故还原)：上游返回 403(余额不足/凭证失效)应触发故障切换，
// 而非把 403 当「成功」透传给客户端。旧实现只把 5xx/429 当失败，导致
// 余额耗尽的首选上游持续 403、永不切换到有余额的备用上游。
func TestForward403Failover(t *testing.T) {
	var aHits, bHits int32
	// 上游 A：优先级高，但余额不足返回 403
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aHits, 1)
		w.WriteHeader(403)
		io.WriteString(w, `{"code":"INSUFFICIENT_BALANCE"}`)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHits, 1)
		w.WriteHeader(200)
		io.WriteString(w, `{"text":"from-B"}`)
	}))
	defer srvB.Close()

	ups := []*upstream.Upstream{
		{ID: 1, Name: "A", BaseURL: srvA.URL, APIKey: "k", Priority: 10, Enabled: true},
		{ID: 2, Name: "B", BaseURL: srvB.URL, APIKey: "k", Priority: 20, Enabled: true},
	}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	fwd.Forward(rec, req, []byte(`{}`), 0, "")

	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("A 返回 403 应切到 B 得 200，实际 %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "from-B") {
		t.Fatalf("应切到 B，实际:\n%s", body)
	}
	if atomic.LoadInt32(&aHits) != 1 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("A 应被试 1 次、B 1 次，实际 A=%d B=%d", aHits, bHits)
	}
}

// countingHealth 记录 Report 调用次数，并兼当 Picker 返回固定上游(避开 exclude)，
// 用于断言「客户端断连不污染健康」。
type countingHealth struct {
	reports int32
	up      *upstream.Upstream
}

func (c *countingHealth) Report(id int64, model string, ok bool, latencyMs int64) {
	atomic.AddInt32(&c.reports, 1)
}
func (c *countingHealth) PickExcluding(groupID int64, model string, exclude map[int64]bool) (*upstream.Upstream, error) {
	if exclude[c.up.ID] {
		return nil, io.EOF // 没有更多可用上游
	}
	return c.up, nil
}

// H2 回归：客户端中途断连(ctx canceled)时，转发层不得 Report 失败——
// 否则一次 abort 会让本次试过的多个健康上游各记一次失败、累积误熔断。
func TestForwardClientCancelNoReport(t *testing.T) {
	// 上游收到请求后阻塞不返回，制造「客户端先取消」的时序窗口。
	gotReq := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(gotReq)
		<-block // 一直阻塞到测试结束
	}))
	defer srv.Close()
	defer close(block)

	ch := &countingHealth{up: &upstream.Upstream{ID: 1, Name: "A", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	fwd := New(ch, ch, noopLogger{}, 3)

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		fwd.Forward(rec, req, []byte(`{"model":"m"}`), 0, "")
		close(done)
	}()
	<-gotReq // 确保请求已发到上游、正阻塞在等响应头
	cancel() // 客户端断连
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("客户端取消后 Forward 应尽快返回")
	}
	if n := atomic.LoadInt32(&ch.reports); n != 0 {
		t.Fatalf("客户端断连不应 Report 任何健康反馈，实际 Report %d 次", n)
	}
}

// H3 回归：单条 SSE data 行超 1MB(旧 Scanner 上限) 也应完整透传、不静默截断。
func TestForwardSSELongLineNoTruncate(t *testing.T) {
	huge := strings.Repeat("x", 3*1024*1024) // 3MB，远超旧 Scanner 1MB 上限
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, "data: "+huge+"\n")
		fl.Flush()
		io.WriteString(w, "data: [DONE]\n")
		fl.Flush()
	}))
	defer srv.Close()

	ups := []*upstream.Upstream{{ID: 1, Name: "A", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(3, time.Hour)
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 3)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0, "")

	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), huge) {
		t.Fatalf("超长单行应完整透传，实际长度 %d 不含完整 payload", len(body))
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Fatal("超长行后续事件([DONE])也应到达，证明未静默截断")
	}
}
type flushRecorder struct {
	header  http.Header
	w       *io.PipeWriter
	flushed chan struct{}
}

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) WriteHeader(int)     {}
func (f *flushRecorder) Write(b []byte) (int, error) {
	return f.w.Write(b)
}
func (f *flushRecorder) Flush() {
	select {
	case f.flushed <- struct{}{}:
	default:
	}
}

// 端到端回归(核心 503 修复)：主力上游恢复到半开后，codex 式并发突发请求
// 应全部 200、零 503。复现并验证「半开放行」修复——旧单闸门设计下并发只 1 个能进、
// 其余在选路阶段拿不到上游而 503(no upstream available)。
func TestForwardHalfOpenConcurrentNo503(t *testing.T) {
	// 健康主力上游：始终 200，带真实时延拉长并发重叠窗口
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	ups := []*upstream.Upstream{{ID: 1, Name: "primary", BaseURL: srv.URL, APIKey: "k", Priority: 1, Enabled: true}}
	hm := health.New(1, 20*time.Millisecond) // 短冷却便于进入半开恢复窗口
	fwd := New(scheduler.New(func(int64) []*upstream.Upstream { return ups }, hm), hm, noopLogger{}, 0)

	// 主力先熔断（模拟一次抖动开了熔断器），等冷却到期 → 下次判定翻半开恢复期
	hm.Report(1, "gpt-5.5", false, 0) // 模型级 OPEN
	time.Sleep(30 * time.Millisecond) // 冷却到期

	body := []byte(`{"model":"gpt-5.5","stream":false}`)
	var ok200, got503 int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // codex 式并发突发
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			fwd.Forward(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))), body, 0, "")
			switch rec.Code {
			case 200:
				atomic.AddInt64(&ok200, 1)
			case 503:
				atomic.AddInt64(&got503, 1)
			}
		}()
	}
	wg.Wait()
	if got503 != 0 {
		t.Fatalf("半开恢复期 8 并发不应有 503，实际 200=%d 503=%d", ok200, got503)
	}
	if ok200 != 8 {
		t.Fatalf("半开恢复期 8 并发应全部 200(主力健康)，实际仅 %d", ok200)
	}
}
