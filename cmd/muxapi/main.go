package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	// 探测用：全部上游（探测是上游级，与分组无关）
	listAll := func() []*upstream.Upstream {
		ups, _ := st.List()
		return ups
	}

	// 组装四层
	hm := health.New(cfg.FailThreshold, cfg.Cooldown)
	sched := scheduler.New(listByGroup, hm)
	fwd := forward.New(sched, hm, st, cfg.MaxRetries)
	mon := monitor.New()
	monitorInterval := func() time.Duration {
		if d, err := time.ParseDuration(st.GetSetting("monitor_interval", "")); err == nil && d > 0 {
			return d
		}
		return cfg.MonitorInterval
	}
	monProber := monitor.NewProber(mon, st, monitorInterval, cfg.ProbePath)
	srv := server.New(fwd, cfg.AdminToken, st, hm, mon, monProber)

	// 收到 SIGINT/SIGTERM 时取消：停探测并触发优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 路由用：上游级主动探测，驱动熔断器（间隔页面可配，缺省回退 config）
	probeInterval := func() time.Duration {
		if d, err := time.ParseDuration(st.GetSetting("probe_interval", "")); err == nil && d > 0 {
			return d
		}
		return cfg.ProbeInterval
	}
	go health.NewProber(hm, listAll, probeInterval, cfg.ProbeModel, cfg.ProbePath).Run(ctx)
	// 看板用：监控项级主动探测，记录成功率/延迟/趋势
	go monProber.Run(ctx)

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
