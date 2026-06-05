package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestProberDetectsDownAndRecover(t *testing.T) {
	var down atomic.Bool // 用开关模拟上游挂/恢复
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	up := &upstream.Upstream{ID: 1, Name: "A", BaseURL: srv.URL, APIKey: "k", Enabled: true}
	m := New(1, 30*time.Millisecond) // 失败1次即熔断
	p := NewProber(m, func() []*upstream.Upstream { return []*upstream.Upstream{up} },
		func() time.Duration { return 20 * time.Millisecond },
		func() string { return "claude-test" }, func() string { return "/v1/chat/completions" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// 初始健康
	time.Sleep(40 * time.Millisecond)
	if !m.IsAvailable(1) {
		t.Fatal("探测应判定健康")
	}
	// 模拟挂掉 → 探测应发现并熔断
	down.Store(true)
	if !waitFor(func() bool { return !m.IsAvailable(1) }, 300*time.Millisecond) {
		t.Fatal("挂掉后探测应发现并熔断")
	}
	// 模拟恢复 → 探测应发现并解熔断（自动回切的基础）
	down.Store(false)
	if !waitFor(func() bool { return m.IsAvailable(1) }, 300*time.Millisecond) {
		t.Fatal("恢复后探测应发现并解熔断 ← failback 基础")
	}
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
