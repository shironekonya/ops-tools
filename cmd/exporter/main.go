// Ops Exporter：读取运维任务执行结果（JSON 文件），转换为 Prometheus 指标。
//
// 当前阶段（TODO 1~8）包含：
//   - 启动一个 HTTP Server
//   - 提供 /healthz 健康检查接口
//   - 提供 /metrics 指标接口（Prometheus 抓取点）
//   - 后台周期刷新任务结果目录，驱动指标更新
//   - 从 JSON 配置文件读取监听地址、结果目录、刷新间隔、日志级别
//   - 输出基本的启动日志
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"ops-exporter/internal/exporter/config"
	"ops-exporter/internal/exporter/expiry"
	"ops-exporter/internal/exporter/ingest"
	"ops-exporter/internal/exporter/metrics"
	"ops-exporter/internal/exporter/refresher"
	"ops-exporter/internal/exporter/store"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// main 负责加载配置、启动 HTTP Server 和刷新循环，并处理退出信号。
func main() {
	// -config 指定配置文件路径；文件不存在时全部走默认值（见 LoadConfig）。
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		// 配置非法属于启动期致命错误：此时日志系统还没按配置建好，
		// 用默认 logger 输出到 stderr 即可。
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}

	// 日志级别来自配置，默认 info。
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	slog.Info("loaded config",
		"config", *configPath,
		"listen_addr", cfg.ListenAddr,
		"result_dir", cfg.ResultDir,
		"refresh_interval", cfg.RefreshInterval.String(),
		"log_level", cfg.LogLevel.String(),
		"max_ingest_body_bytes", cfg.MaxIngestBodyBytes,
		"max_build_age", cfg.MaxBuildAge.String(),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)

	// 使用独立 registry 而不是 prometheus.DefaultRegisterer：
	// 一是避免和第三方库默认注册的指标混在一起，
	// 二是让 /metrics 的输出只包含本服务主动暴露的内容。
	reg := prometheus.NewRegistry()
	// Go 运行时指标（goroutine 数、堆内存等），排查 exporter 自身问题时有用。
	reg.MustRegister(collectors.NewGoCollector())

	jobMetrics, err := metrics.NewJobMetrics(reg)
	if err != nil {
		// 指标注册失败属于编程错误，启动阶段就必须暴露出来。
		slog.Error("register metrics failed", "error", err)
		os.Exit(1)
	}

	// 推送管道（Task 2）：内存 store + jenkins_build_* 指标 + HTTP 接收入口。
	// 与文件管道共享 registry/mux/logger，但数据互相隔离（ops_job_* vs jenkins_build_*）。
	buildMetrics, err := metrics.NewBuildMetrics(reg)
	if err != nil {
		slog.Error("register build metrics failed", "error", err)
		os.Exit(1)
	}
	buildStore := &store.BuildStore{}
	ingestHandler := ingest.NewIngestHandler(buildStore, buildMetrics, cfg.MaxIngestBodyBytes, logger)
	mux.Handle("/api/v1/ingest/build", ingestHandler)
	slog.Info("ingest endpoint enabled", "path", "/api/v1/ingest/build")

	// 数据过期清理器（Task 5）：与刷新循环共享 ctx/优雅退出，禁用时 Run 立即返回。
	expirer := expiry.New(buildStore, buildMetrics, cfg.MaxBuildAge, logger)

	// runCtx 控制后台刷新 goroutine 的生命周期：
	// 收到退出信号后 cancel，刷新循环在下一次 select 时退出。
	runCtx, cancelRun := context.WithCancel(context.Background())

	// WaitGroup 用于确认所有后台 goroutine 真正退出后再结束进程，
	// 避免退出瞬间它们还在更新指标造成数据竞争。
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		refresher.RunRefresher(runCtx, cfg.ResultDir, cfg.RefreshInterval, jobMetrics)
	}()
	go func() {
		defer wg.Done()
		expirer.Run(runCtx, cfg.ExpiryInterval)
	}()

	// /metrics 直接由 registry 生成文本格式，
	// handler 每次抓取时从 registry 收集当前值，因此指标更新无需通知 handler。
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	// HTTP Server 超时（Task 5）：全部来自配置，缺省为安全默认值。
	// 没有 ReadHeaderTimeout 时，恶意客户端可以连上不发请求头长期占用连接（Slowloris）。
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}

	// 在独立 goroutine 中启动服务，主 goroutine 等待退出信号。
	errCh := make(chan error, 1)
	go func() {
		slog.Info("ops exporter starting", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 监听 Ctrl+C 和系统终止信号，保证退出时优雅关闭连接。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		slog.Error("server failed", "error", err)
		os.Exit(1)
	case sig := <-stop:
		slog.Info("shutting down", "signal", sig.String())
	}

	// 优雅退出顺序：先停后台刷新，确认它退出，再关闭 HTTP Server。
	// 顺序反过来会出现"刷新 goroutine 还活着但进程已退出"的窗口。
	cancelRun()
	wg.Wait()

	// 给在途请求一段有限时间完成（可配，默认 5s），超时则强制关闭 Server。
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "error", err)
		os.Exit(1)
	}
	slog.Info("ops exporter stopped")
}

// handleHealthz 返回服务健康状态，供存活探针和人工检查使用。
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// 写失败的唯一原因是客户端提前断开（探针超时等），
	// 没有可恢复的动作，记录日志也只会刷屏，因此明确忽略。
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
