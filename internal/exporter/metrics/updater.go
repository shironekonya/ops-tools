package metrics

import "ops-exporter/internal/exporter/model"

// 本文件实现 TODO 5：任务结果到 Prometheus 指标的转换。
//
// 数据流：model.TaskResult --Update--> 四个 GaugeVec 的对应序列
//                              （ApplyResults 负责批量入口和重复任务去重）

// 转换逻辑只操作 JobMetrics 暴露的 *GaugeVec 方法，
// 不需要直接引用 prometheus 包。

// Update 把一条任务结果写入指标。
//
// 更新规则：
//   - ops_job_last_status:                success→1，failed→0，每次都更新
//   - ops_job_last_run_timestamp_seconds: 任务结束时间，每次都更新
//   - ops_job_last_duration_seconds:      本次执行耗时，每次都更新
//   - ops_job_last_success_timestamp_seconds: 只有成功才更新；
//     失败时既不覆盖旧值，也要保证序列存在（首次为 0）
//
// 失败时不覆盖 last_success 的实现方式：
// 对 GaugeVec 调用 WithLabelValues 时，若该 job 的序列还不存在，
// client_golang 会创建它并把值保持为 0（Gauge 初始值）；
// 若序列已存在则原样返回，只要不再调用 Set，旧的成功时间就保留。
// 因此失败分支只调 WithLabelValues、不调 Set，正好实现两种语义。
//
// 并发安全：GaugeVec 内部自带锁，Set/WithLabelValues 都是原子的，
// 本方法可以被刷新 goroutine 和其他 goroutine 同时调用（TODO 6 会用到）。
func (m *JobMetrics) Update(r model.TaskResult) {
	job := r.JobName

	status := 0.0
	if r.Status == model.StatusSuccess {
		status = 1
	}
	m.LastStatus.WithLabelValues(job).Set(status)

	m.LastRunTimestamp.WithLabelValues(job).Set(float64(r.EndTime.Unix()))
	m.LastDuration.WithLabelValues(job).Set(r.Duration().Seconds())

	if r.Status == model.StatusSuccess {
		m.LastSuccessTimestamp.WithLabelValues(job).Set(float64(r.EndTime.Unix()))
	} else {
		// 失败：确保序列存在（首次为 0），但不改动已有成功时间。
		m.LastSuccessTimestamp.WithLabelValues(job)
	}
}

// ApplyResults 把一批任务结果写入指标，是 TODO 6 周期刷新的入口。
//
// 同一个 job_name 在目录里可能对应多个文件（任务方重复写入），
// 这里按 end_time 保留最新的一条，旧的视为过期数据直接忽略；
// 过期与否只影响本次批量更新，不影响其他任务。
func (m *JobMetrics) ApplyResults(results []model.TaskResult) {
	latest := make(map[string]model.TaskResult, len(results))
	for _, r := range results {
		if prev, ok := latest[r.JobName]; ok && r.EndTime.Before(prev.EndTime) {
			continue // 比已采纳的结果旧，跳过
		}
		latest[r.JobName] = r
	}

	for _, r := range latest {
		// map 遍历顺序随机，但各任务更新的是不同的标签序列，互不影响。
		m.Update(r)
	}
}
