// Jenkins Collector：定时从 Jenkins 采集流水线构建数据，标准化后 POST 给 Custom Exporter。
//
// 流程：config → jenkins.Client → sender.Sender → collector.Collector.Run(ticker)
// 认证信息走配置文件或环境变量（JENKINS_USERNAME / JENKINS_API_TOKEN），不硬编码。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ops-exporter/internal/collector/collector"
	"ops-exporter/internal/collector/config"
	"ops-exporter/internal/collector/jenkins"
	"ops-exporter/internal/collector/sender"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	slog.Info("loaded config",
		"jenkins_url", cfg.JenkinsURL,
		"jobs", cfg.Jobs,
		"exporter_url", cfg.ExporterURL,
		"interval", cfg.Interval.String(),
		"timeout", cfg.Timeout.String(),
		"max_retries", cfg.MaxRetries,
		"state_file", cfg.StateFile,
	)

	jclient := jenkins.NewClient(cfg.JenkinsURL, cfg.Username, cfg.APIToken, cfg.Timeout)
	s := sender.NewSender(cfg.ExporterURL, cfg.Timeout, cfg.MaxRetries, logger)
	c := collector.NewWithState(jclient, s, cfg.Jobs, logger, cfg.StateFile)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, cfg.Interval)
	}()

	// 优雅退出：收到信号后 cancel，等待采集循环退出。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		slog.Info("shutting down", "signal", sig.String())
	case <-ctx.Done():
	}

	cancel()
	<-done
	slog.Info("collector stopped")
}
