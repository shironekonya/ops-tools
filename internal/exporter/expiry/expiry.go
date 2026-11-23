// Package expiry 实现 Task 5 的"数据过期"：定期清理超过 maxAge 未更新的 Build，
// 并删除对应的 jenkins_build_* Gauge 序列，避免僵尸 Job 的陈旧状态长期驻留。
//
// 为什么独立成包而不是塞进 refresher：
//   - refresher 负责"文件来源"的周期刷新，数据源和语义都不同；
//   - 过期清理只针对推送来源的 BuildStore/BuildMetrics，职责单一；
//   - 两者都受 main 的同一个 ctx 管理、共享优雅退出顺序。
package expiry

import (
	"context"
	"log/slog"
	"time"

	"ops-exporter/internal/exporter/metrics"
	"ops-exporter/internal/exporter/store"
)

// Expirer 执行一次或周期的过期清理。
type Expirer struct {
	store   *store.BuildStore
	metrics *metrics.BuildMetrics
	maxAge  time.Duration
	logger  *slog.Logger
}

// New 创建清理器。maxAge<=0 表示禁用清理（向后兼容：不配置就不会删数据）。
func New(s *store.BuildStore, m *metrics.BuildMetrics, maxAge time.Duration, logger *slog.Logger) *Expirer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Expirer{store: s, metrics: m, maxAge: maxAge, logger: logger}
}

// Run 启动周期清理循环：先立即清一次，之后每 interval 一次，ctx 取消时退出。
// maxAge<=0 时直接返回（功能禁用），不启动 ticker。
func (e *Expirer) Run(ctx context.Context, interval time.Duration) {
	if e.maxAge <= 0 {
		e.logger.Info("build expiry disabled")
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}

	e.PruneOnce(time.Now())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("build expiry stopped")
			return
		case now := <-ticker.C:
			e.PruneOnce(now)
		}
	}
}

// PruneOnce 执行一次清理，返回被清理的 job 数。
// now 注入便于测试精确控制过期边界；生产由 Run 传 time.Now()。
func (e *Expirer) PruneOnce(now time.Time) int {
	removed := e.store.Prune(e.maxAge, now)
	for _, job := range removed {
		e.metrics.DeleteJob(job)
		e.logger.Info("expired build data removed", "job", job, "max_age", e.maxAge.String())
	}
	return len(removed)
}
