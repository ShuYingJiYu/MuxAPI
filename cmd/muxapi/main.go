// Command muxapi 组装网关各层，并管理 HTTP 服务与后台任务的生命周期。
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mirainya/muxapi/internal/billing"
	"github.com/mirainya/muxapi/internal/config"
	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/server"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

func main() {
	cfg := config.Load()
	if cfg.AdminToken == "" {
		slog.Warn("MUXAPI_TOKEN 未设置：管理后台无鉴权，切勿对外暴露")
	}

	st, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		slog.Error("open store failed", "err", err)
		return
	}
	defer st.Close()

	// 调度用：某分组下启用的上游（实时查库，后台增删即时生效）
	listByGroup := func(groupID int64) []*upstream.Upstream {
		ups, err := st.ListEnabledByGroup(groupID)
		if err != nil {
			slog.Error("list upstreams failed", "err", err)
			return nil
		}
		return ups
	}

	// 依赖顺序：健康状态 -> 调度 -> 转发 -> 主动监控 -> HTTP 接入。
	hm := health.New(cfg.FailThreshold, cfg.Cooldown)
	// 重启恢复：用最近的转发样本重建选路预估(延迟/成功率 EWMA)，不重建熔断状态
	if samples, err := st.RecentSamples(2000); err != nil {
		slog.Warn("seed route stats from logs failed", "err", err)
	} else if len(samples) > 0 {
		hs := make([]health.RouteSample, len(samples))
		for i, s := range samples {
			hs[i] = health.RouteSample{UpstreamID: s.UpstreamID, Model: s.Model, OK: s.OK, LatencyMs: s.LatencyMs}
		}
		hm.Seed(hs)
		slog.Info("seeded route stats from logs", "samples", len(hs))
	}
	sched := scheduler.New(listByGroup, hm)
	fwd := forward.New(sched, hm, cfg.MaxRetries)
	mon := monitor.New(st)
	settingDuration := func(key string, def time.Duration) func() time.Duration {
		return func() time.Duration {
			if d, err := time.ParseDuration(st.GetSetting(key, "")); err == nil && d > 0 {
				return d
			}
			return def
		}
	}
	settingString := func(key, def string) func() string {
		return func() string {
			if v := st.GetSetting(key, ""); v != "" {
				return v
			}
			return def
		}
	}
	settingInt := func(key string, def int) func() int {
		return func() int {
			if n, err := strconv.Atoi(st.GetSetting(key, "")); err == nil && n > 0 {
				return n
			}
			return def
		}
	}
	// 仅在收到首个响应字节前允许超时换源；流开始后保持透明转发，由客户端取消请求。
	firstResponseTimeoutMs := settingInt("first_response_timeout_ms", 120000)
	fwd.SetFirstResponseTimeout(func() time.Duration {
		return time.Duration(firstResponseTimeoutMs()) * time.Millisecond
	})

	// 健康事件主动告警：熔断翻转时推送 Webhook（URL 空则关闭）。
	// id→name 解析用现成 List()，解析不到回退 id 字符串。
	upstreamName := func(id int64) string {
		ups, _ := st.List()
		for _, u := range ups {
			if u.ID == id {
				return u.Name
			}
		}
		return ""
	}
	hm.SetAlerter(health.NewWebhookAlerter(
		settingString("alert_webhook", ""),
		settingDuration("alert_debounce", 60*time.Second),
		upstreamName,
	))
	// 探测系统统一：monitor 探测器是唯一主动探测源，注入 hm 后一次探测双写
	//（看板统计 + 路由熔断器）。探测间隔/路径已全下放到各监控项，
	// 传 nil 让 prober 用内置默认（5m / /v1/chat/completions），监控项可逐项覆盖。
	monProber := monitor.NewProber(mon, st, hm, nil, nil)
	billingMgr := billing.NewManager(st)
	srv := server.New(fwd, cfg.AdminToken, st, hm, mon, monProber, cfg.MaxBody)
	srv.SetBillingManager(billingMgr)

	// 收到 SIGINT/SIGTERM 时取消：停探测并触发优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 后台 goroutine 用 WaitGroup 跟踪：Shutdown 后等它们退出再 st.Close()，
	// 消除退出期探测/清理仍在写库而 DB 已关的竞态。
	var wg sync.WaitGroup
	// 监控项级主动探测：记看板(成功率/延迟/趋势) + 驱动路由熔断器。
	wg.Add(1)
	go func() {
		defer wg.Done()
		monProber.Run(ctx)
	}()
	// Provider billing collection is low-frequency and isolated from forwarding.
	wg.Add(1)
	go func() {
		defer wg.Done()
		billingMgr.Run(ctx)
	}()
	// 请求审计按天保留，默认 7 天；每 10 分钟分批删除过期请求及其尝试链。
	requestRetentionDays := settingInt("request_retention_days", 7)
	wg.Add(1)
	go func() {
		defer wg.Done()
		runLogJanitor(ctx, st, requestRetentionDays)
	}()

	// 防 slowloris：仅限制读 header 的时长，不设全局 ReadTimeout——
	// 否则会误杀慢上传/流式上传。MaxHeaderBytes 限制 header 体积。
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server exited", "err", err)
			stop()
		}
	}()
	slog.Info("muxapi starting", "addr", cfg.Addr)

	<-ctx.Done() // 等信号
	slog.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		slog.Error("shutdown failed", "err", err)
	}
	wg.Wait() // 等后台 goroutine 退出，再让 defer st.Close() 安全关库
}

// runLogJanitor 定时清理请求审计与探测结果：启动先清一次，之后每 10 分钟一轮。
// keepDays() 每轮读取最新保留天数；每批最多 5000 个请求，避免长事务影响业务查询。
// 探测结果固定保留 48h（覆盖看板 24h 展示窗口有余），防 probe_results 表无限增长。
func runLogJanitor(ctx context.Context, st *store.Store, keepDays func() int) {
	const (
		probeKeepHours = 48
		requestBatch   = 5000
		maxBatches     = 10
	)
	prune := func() {
		days := keepDays()
		if days > 0 {
			for batch := 0; batch < maxBatches; batch++ {
				deleted, err := st.PruneRequests(days, requestBatch)
				if err != nil {
					slog.Error("request janitor prune failed", "err", err)
					break
				}
				if deleted > 0 {
					slog.Info("request janitor pruned", "deleted", deleted, "keepDays", days)
				}
				if deleted < requestBatch {
					break
				}
			}
		}
		if deleted, err := st.PruneProbes(probeKeepHours); err != nil {
			slog.Error("probe janitor prune failed", "err", err)
		} else if deleted > 0 {
			slog.Info("probe janitor pruned", "deleted", deleted, "keepHours", probeKeepHours)
		}
	}
	prune() // 启动即清一次，立刻收敛历史堆积
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
