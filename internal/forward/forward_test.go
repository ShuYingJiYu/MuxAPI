package forward

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/upstream"
)

// noopLogger 测试用空日志器
type noopLogger struct{}

func (noopLogger) Log(groupID, upstreamID int64, status int, latencyMs int64) {}

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
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0)

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
	fwd.Forward(rec, req, []byte(`{"stream":true}`), 0)

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
	if !hm.IsAvailable(2) || hm.IsAvailable(1) {
		t.Fatal("失败后 A 应熔断、B 应可用")
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
	go fwd.Forward(rec, req, []byte(`{"stream":true}`), 0)

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

// flushRecorder 把写入接到 io.Pipe，并记录每次 Flush，用于验证增量透传。
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
