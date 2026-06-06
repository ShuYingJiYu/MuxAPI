package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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

	st, err := store.Open(cfg.DBPath)
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

	// 组装四层
	hm := health.New(cfg.FailThreshold, cfg.Cooldown)
	sched := scheduler.New(listByGroup, hm)
	fwd := forward.New(sched, hm, st, cfg.MaxRetries)
	mon := monitor.New()
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
	srv := server.New(fwd, cfg.AdminToken, st, hm, mon, monProber)

	// 收到 SIGINT/SIGTERM 时取消：停探测并触发优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 监控项级主动探测：记看板(成功率/延迟/趋势) + 驱动路由熔断器。
	go monProber.Run(ctx)
	// 日志清理：按条数保留最新 N 条，定时裁剪防止 logs 表无限增长（页面可配，缺省 1 万条）。
	logRetention := settingInt("log_retention", 10000)
	go runLogJanitor(ctx, st, logRetention)

	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}
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
}

// runLogJanitor 定时裁剪 logs 表：启动先清一次，之后每 10 分钟一轮。
// keep() 每轮取最新值，页面改保留条数下一轮生效；返回 0 表示关闭清理。
func runLogJanitor(ctx context.Context, st *store.Store, keep func() int) {
	prune := func() {
		n := keep()
		if n <= 0 {
			return
		}
		if deleted, err := st.PruneLogs(n); err != nil {
			slog.Error("log janitor prune failed", "err", err)
		} else if deleted > 0 {
			slog.Info("log janitor pruned", "deleted", deleted, "keep", n)
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
