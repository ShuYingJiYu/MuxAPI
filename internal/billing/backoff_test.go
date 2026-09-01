package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

// TestManagerRateLimitBackoff：上游 429 后，同一 upstream 在冷却期内不会再被 RefreshAll 打。
// 修法防的是 [[muxapi 生产 pod 每 10min 撞 429 刷 WARN 日志]] 的死撞循环。
func TestManagerRateLimitBackoff(t *testing.T) {
	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usage/token/" {
			hits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.NotFound(w, r)
	}))
	defer provider.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "backoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{
		Name: "throttled", BaseURL: provider.URL, APIKey: "sk-test",
		BillingType: upstream.BillingNewAPI, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}

	m := NewManager(st)
	// 首次调用：应命中 upstream 一次，收到 429，进入冷却
	_, firstErr := m.Refresh(context.Background(), u.ID)
	if !errors.Is(firstErr, ErrRateLimited) {
		t.Fatalf("first refresh: want ErrRateLimited, got %v", firstErr)
	}
	// 手工写入冷却记录（Refresh 走的是 refreshItem 直接返回错，不走 RefreshAll 的冷却写入路径）
	m.markRateLimited(u.ID)
	if !m.inCoolDown(u.ID) {
		t.Fatalf("upstream should be in cool-down after markRateLimited")
	}

	// RefreshAll 应跳过冷却中的 upstream，不再打上游
	hitsBefore := hits.Load()
	m.RefreshAll(context.Background())
	if got := hits.Load() - hitsBefore; got != 0 {
		t.Fatalf("RefreshAll should skip cool-down upstream, upstream hit %d times", got)
	}
}

// TestManagerCoolDownExpires：冷却时间过后，允许再次尝试。
func TestManagerCoolDownExpires(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "expire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m := NewManager(st)
	// 直接写一个已过期的冷却，验证 inCoolDown 返回 false 且清理条目
	m.coolDownMu.Lock()
	m.coolDown[42] = time.Now().Add(-time.Minute)
	m.coolDownMu.Unlock()
	if m.inCoolDown(42) {
		t.Fatal("expired cool-down should not block")
	}
	m.coolDownMu.Lock()
	_, still := m.coolDown[42]
	m.coolDownMu.Unlock()
	if still {
		t.Fatal("inCoolDown should clean up expired entries")
	}
}
