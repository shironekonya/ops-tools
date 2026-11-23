package refresher

// 本文件实现 TODO 6（周期刷新）与 TODO 9（日志降噪）。
//
// 数据流（后台 goroutine 内循环）：
//
//	reader.LoadTaskResults(dir) ──fileErrs──> refreshTracker.reportFiles
//	        │                              ├─ 首次失败 → slog.Warn
//	        │                              ├─ 重复失败 → slog.Debug（带连续次数）
//	        │                              └─ 不再失败 → 静默移出跟踪
//	        └──results──> JobMetrics.ApplyResults ──> /metrics（内存，Prometheus 随时来抓）
//
// 目录级错误由 reportDir 处理：首次 Error、重复 Debug、恢复 Info。
// 关键取舍不变：抓取（读内存）和刷新（读磁盘）解耦，
// Prometheus 抓取频率再高也不会放大文件 I/O。

import (
	"context"
	"log/slog"
	"ops-exporter/internal/exporter/metrics"
	"ops-exporter/internal/exporter/reader"
	"time"
)

// refreshTracker 跟踪文件级和目录级错误的连续发生情况，用于日志降噪。
//
// 降噪规则：
//   - 同一个问题连续出现时，只有第一次用高级别（Warn/Error）输出，
//     之后降为 Debug 并附连续次数 —— 告警只响一次，排查线索不丢；
//   - 问题消失时：文件级静默移出（无法区分"内容修好了"还是"文件删了"，
//     不猜就不发日志）；目录级恢复发 Info（"目录重新可读"语义明确）。
//
// 并发约定：只被刷新 goroutine 单线程使用，不需要加锁。
type refreshTracker struct {
	fileFailures map[string]int
	dirFailures  int
}

func newRefreshTracker() *refreshTracker {
	return &refreshTracker{fileFailures: make(map[string]int)}
}

// reportFiles 记录本轮读取到的文件级错误。
func (t *refreshTracker) reportFiles(fes []reader.FileError) {
	failed := make(map[string]bool, len(fes))
	for _, fe := range fes {
		failed[fe.Path] = true
		t.fileFailures[fe.Path]++
		n := t.fileFailures[fe.Path]
		if n == 1 {
			slog.Warn("invalid task result file",
				"path", fe.Path, "error", fe.Err)
		} else {
			slog.Debug("invalid task result file",
				"path", fe.Path, "error", fe.Err, "consecutive_failures", n)
		}
	}
	for path := range t.fileFailures {
		if !failed[path] {
			delete(t.fileFailures, path)
		}
	}
}

// reportDir 记录目录级刷新结果，err 为 nil 表示本轮目录读取成功。
func (t *refreshTracker) reportDir(err error) {
	if err == nil {
		if t.dirFailures > 0 {
			slog.Info("task result directory readable again")
		}
		t.dirFailures = 0
		return
	}
	t.dirFailures++
	if t.dirFailures == 1 {
		slog.Error("refresh failed", "error", err)
	} else {
		slog.Debug("refresh failed",
			"error", err, "consecutive_failures", t.dirFailures)
	}
}

// refreshOnce 执行一轮刷新：读取目录、上报文件级错误、写入指标。
//
// 返回的 error 只代表目录级失败（目录不存在、无权限等），
// 交给 tracker 节流后输出；单个文件的问题不会走到这个返回值，
// 也不影响其他任务和本轮已成功解析的部分。
// tracker 必须非 nil（由 RunRefresher 创建并持有）。
func refreshOnce(dir string, metrics *metrics.JobMetrics, tracker *refreshTracker) error {
	start := time.Now()

	results, fileErrs, err := reader.LoadTaskResults(dir)
	if err != nil {
		return err
	}

	tracker.reportFiles(fileErrs)
	metrics.ApplyResults(results)

	// 只有整轮刷新成功才更新：如果目录持续读不了，
	// 这个时间戳会停止前进，PromQL 用 time() - 它 就能发现 exporter 自身失效。
	metrics.LastRefresh.Set(float64(time.Now().Unix()))

	// 成功刷新本身是 Debug：正常运行时每轮都 Info 会刷屏，
	// 排查问题时把日志级别调到 debug 就能看到每轮细节。
	slog.Debug("refresh completed",
		"dir", dir,
		"results", len(results),
		"file_errors", len(fileErrs),
		"elapsed", time.Since(start).String(),
	)
	return nil
}

// RunRefresher 以固定间隔周期刷新，直到 ctx 被取消。
// 这是阻塞调用，调用方应在独立 goroutine 中运行它，
// 并通过 ctx 取消来停止（配合 WaitGroup 确认退出）。
//
// 目录级错误不会终止循环：目录可能暂时被卸载或权限临时变化，
// 下个周期重试是更合理的恢复策略，退出交给 ctx 控制。
func RunRefresher(ctx context.Context, dir string, interval time.Duration, metrics *metrics.JobMetrics) {
	tracker := newRefreshTracker()

	// 启动后立即刷一次，指标不必等第一个 tick 就有数据。
	if err := refreshOnce(dir, metrics, tracker); err != nil {
		tracker.reportDir(err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("refresh loop stopped")
			return
		case <-ticker.C:
			if err := refreshOnce(dir, metrics, tracker); err != nil {
				tracker.reportDir(err)
			}
		}
	}
}
