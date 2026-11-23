package metrics

import "ops-exporter/internal/exporter/model"

// 本文件实现 model.BuildResult → jenkins_build_* 指标的转换，对齐 updater.go 的模式。
//
// 数据流：model.BuildResult --Update--> 三个 GaugeVec + 两个 CounterVec
//
// 与文件管道 updater.go 的关键差异：
//   - 文件管道每轮全量重读，Counter 语义不用它；推送是事件式的，
//     每收到一个**被接受**的新 Build 就给 Counter +1（重复/旧 Build 不计数）。
//   - 因此"是否调用 Update"由调用方（ingest handler）依据 BuildStore.Save
//     的返回值决定；本文件只负责"给定一条合法 Build，如何写指标"。

// Update 把一条被接受的构建结果写入指标。
//
// 更新规则：
//   - jenkins_build_status:            success→1，其余→0，每次更新
//   - jenkins_build_last_timestamp_seconds: 构建结束时刻，每次更新
//   - jenkins_build_duration_seconds:        本次耗时，每次更新
//   - jenkins_build_total:                   每次调用 +1
//   - jenkins_build_failure_total:           仅 status=failed 时 +1
//
// 并发安全：GaugeVec/CounterVec 内部自带锁，方法均为原子操作，
// 可被多个 ingest 请求 goroutine 同时调用（与 updater.go 注释同理）。
func (m *BuildMetrics) Update(b model.BuildResult) {
	job := b.JobName

	status := 0.0
	if b.Status == model.BuildStatusSuccess {
		status = 1
	}
	m.Status.WithLabelValues(job).Set(status)
	m.LastTimestamp.WithLabelValues(job).Set(float64(b.Timestamp))
	m.Duration.WithLabelValues(job).Set(b.Duration)

	m.Total.WithLabelValues(job).Inc()
	if b.Status == model.BuildStatusFailed {
		m.FailureTotal.WithLabelValues(job).Inc()
	} else {
		// 非失败也要保证序列存在（首次为 0）。
		// 否则从未失败的 job 在 /metrics 里没有该序列，Grafana 会显示 No data
		// 而不是 0——与 ops_job_last_success_timestamp 的处理同理。
		m.FailureTotal.WithLabelValues(job)
	}
}
