package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

const (
	defaultRefreshInterval = 10 * time.Minute
	defaultRefreshTimeout  = 10 * time.Second
	defaultConcurrency     = 4
	defaultPricingInterval = 24 * time.Hour
	defaultPricingTimeout  = 30 * time.Second
	// rateLimitBackoff 上游 429 后跳过多少个刷新周期。取略大于 interval 的值就够，
	// 3 倍能覆盖 15-30 分钟窗口的上游限流(us.oojj.top 实测约每 15-20 分钟一次)。
	rateLimitBackoff = 3
)

// Manager schedules provider billing collection independently from request forwarding.
type Manager struct {
	store           *store.Store
	interval        time.Duration
	timeout         time.Duration
	concurrency     int
	slots           chan struct{}
	pricingInterval time.Duration
	pricingTimeout  time.Duration
	pricingURL      string
	pricingClient   *http.Client
	pricingFallback []byte

	// coolDown 记录每个 upstream 下次允许再打计费端点的时刻。上游 429 后写入未来
	// 时刻，RefreshAll 提前过滤掉命中冷却期的项，避免死撞并把 WARN 日志刷屏。
	// 只有 429 触发冷却；普通错误(网络断/500) 走原节奏重试。
	coolDownMu sync.Mutex
	coolDown   map[int64]time.Time
}

func NewManager(st *store.Store) *Manager {
	return &Manager{
		store: st, interval: defaultRefreshInterval, timeout: defaultRefreshTimeout,
		concurrency: defaultConcurrency, slots: make(chan struct{}, defaultConcurrency),
		pricingInterval: defaultPricingInterval, pricingTimeout: defaultPricingTimeout,
		pricingURL: defaultPricingURL, pricingClient: http.DefaultClient,
		pricingFallback: embeddedPricingCatalog,
		coolDown:        make(map[int64]time.Time),
	}
}

func (m *Manager) inCoolDown(upstreamID int64) bool {
	m.coolDownMu.Lock()
	defer m.coolDownMu.Unlock()
	until, ok := m.coolDown[upstreamID]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(m.coolDown, upstreamID)
	return false
}

func (m *Manager) markRateLimited(upstreamID int64) {
	m.coolDownMu.Lock()
	m.coolDown[upstreamID] = time.Now().Add(rateLimitBackoff * m.interval)
	m.coolDownMu.Unlock()
}

func (m *Manager) acquire(ctx context.Context) error {
	select {
	case m.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release() { <-m.slots }

func (m *Manager) refreshItem(ctx context.Context, item *upstream.Upstream) (store.BillingStatus, error) {
	if item.BillingType == upstream.BillingNone || item.BillingType == "" {
		return store.BillingStatus{}, ErrBillingDisabled
	}
	if err := m.acquire(ctx); err != nil {
		return store.BillingStatus{}, err
	}
	defer m.release()

	requestCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	result, collectErr := Fetch(requestCtx, item)
	now := time.Now().Unix()
	if collectErr != nil {
		if err := m.store.SaveBillingFailure(item.ID, collectErr.Error(), now); err != nil {
			return store.BillingStatus{}, errors.Join(collectErr, err)
		}
		state, err := m.store.GetBillingStatus(item.ID)
		if err != nil {
			return store.BillingStatus{}, errors.Join(collectErr, err)
		}
		return state, collectErr
	}
	flushCtx, flushCancel := context.WithTimeout(ctx, 2*time.Second)
	flushErr := m.store.FlushRequests(flushCtx)
	flushCancel()
	if flushErr != nil {
		return store.BillingStatus{}, fmt.Errorf("flush request audit before billing snapshot: %w", flushErr)
	}

	status := "ok"
	if result.Warning != "" {
		status = "partial"
	}
	observedAt := result.ObservedAt.Unix()
	if result.ObservedAt.IsZero() {
		observedAt = now
	}
	state := store.BillingStatus{
		UpstreamID: item.ID, Currency: result.Currency, Remaining: result.Remaining,
		Unlimited: result.Unlimited, BillingGroup: result.BillingGroup,
		GroupMultiplier: result.GroupMultiplier, EffectiveMultiplier: result.EffectiveMultiplier,
		ReportedListCost: result.ReportedListCost, ReportedActualCost: result.ReportedActualCost,
		Status: status, Error: result.Warning, ObservedAt: observedAt, RefreshedAt: now,
	}
	if err := m.store.SaveBillingSuccess(state); err != nil {
		return store.BillingStatus{}, err
	}
	return m.store.GetBillingStatus(item.ID)
}

// Refresh updates one upstream immediately and returns the persisted state.
func (m *Manager) Refresh(ctx context.Context, upstreamID int64) (store.BillingStatus, error) {
	item, err := m.store.Get(upstreamID)
	if err != nil {
		return store.BillingStatus{}, err
	}
	return m.refreshItem(ctx, item)
}

// RefreshAll updates every configured billing upstream with a bounded worker pool.
func (m *Manager) RefreshAll(ctx context.Context) {
	items, err := m.store.List()
	if err != nil {
		slog.Warn("list billing upstreams failed", "err", err)
		return
	}
	jobs := make(chan *upstream.Upstream)
	var workers sync.WaitGroup
	workers.Add(m.concurrency)
	for range m.concurrency {
		go func() {
			defer workers.Done()
			for item := range jobs {
				_, err := m.refreshItem(ctx, item)
				if err == nil || errors.Is(err, context.Canceled) {
					continue
				}
				if errors.Is(err, ErrRateLimited) {
					m.markRateLimited(item.ID)
					// 上游限流是常态而非故障，仅记一条 INFO 一次(冷却期内不会再打)
					slog.Info("billing refresh rate limited, backing off",
						"upstream_id", item.ID, "name", item.Name,
						"backoff", rateLimitBackoff*m.interval)
					continue
				}
				slog.Warn("billing refresh failed", "upstream_id", item.ID, "name", item.Name, "err", err)
			}
		}()
	}
	for _, item := range items {
		if item.BillingType == upstream.BillingNone || item.BillingType == "" {
			continue
		}
		if m.inCoolDown(item.ID) {
			continue
		}
		select {
		case jobs <- item:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

// Run refreshes billing and pricing independently on fixed low-frequency intervals.
func (m *Manager) Run(ctx context.Context) {
	if err := m.refreshPricing(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("pricing catalog refresh failed", "err", err)
	}
	m.RefreshAll(ctx)
	billingTicker := time.NewTicker(m.interval)
	pricingTicker := time.NewTicker(m.pricingInterval)
	defer billingTicker.Stop()
	defer pricingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-billingTicker.C:
			m.RefreshAll(ctx)
		case <-pricingTicker.C:
			if err := m.refreshPricing(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("pricing catalog refresh failed", "err", err)
			}
		}
	}
}
