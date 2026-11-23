package metrics

// 本文件实现动态化改造 Task 2 / Task 4 的指标定义：jenkins_build_* 命名空间。
//
// 为什么独立于 metrics.go（ops_job_*）：
//   - 数据来源不同：jenkins_build_* 来自 HTTP 推送的 BuildResult，ops_job_* 来自文件；
//   - 语义不同：Build 有 build_number 维度，job 没有；
//   - Task 4 明确要求 jenkins_build_* 前缀。
// 两者注册到同一个 registry，一次 /metrics 抓取全拿到（见 docs/task1-architecture.md）。
//
// 指标类型选择（对齐任务文档）：
//   - status / last_timestamp / duration：GaugeVec，描述"最近一次构建"，会随新 Build 上下波动；
//   - total / failure_total：CounterVec，累计次数，单调递增。
//
// 标签控制：只用 job_name 一个标签。刻意不作为标签的字段：
//   - build_number：随构建次数无限增长，会让基数爆炸（任务文档明确禁止）；
//   - failed_stage / 日志 / 错误堆栈：内容不可控，属于日志范畴。
// 标签基数 = Jenkins Job 数量，可控。

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// BuildMetrics 保存 Jenkins 构建相关的全部指标。
type BuildMetrics struct {
	// Status 最近一次构建状态：1=success，0=failed/unstable/aborted。
	// 用 1/0 而非四种枚举值，是为了让 Grafana/PromQL 直接复用成功率的语义
	// （sum(status)/count），与 ops_job_last_status 保持风格一致。
	Status *prometheus.GaugeVec

	// LastTimestamp 最近一次构建结束时刻（Unix 秒），来自 BuildResult.timestamp。
	LastTimestamp *prometheus.GaugeVec

	// Duration 最近一次构建耗时（秒）。
	Duration *prometheus.GaugeVec

	// Total 该 job 累计收到的构建次数（Counter，每次被接受的 Build +1）。
	Total *prometheus.CounterVec

	// FailureTotal 该 job 累计失败次数（Counter，仅 status=failed 时 +1）。
	// unstable/aborted 不计入失败（它们语义上不是"构建失败"，见 build.go 注释）。
	FailureTotal *prometheus.CounterVec
}

// NewBuildMetrics 创建全部 jenkins_build_* 指标并注册到给定 registry。
// 注册失败返回错误（重复注册同名指标是编程错误，启动期即暴露），
// 模式与 NewJobMetrics 完全一致。
func NewBuildMetrics(reg prometheus.Registerer) (*BuildMetrics, error) {
	m := &BuildMetrics{
		Status: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jenkins_build_status",
			Help: "Status of the latest Jenkins build: 1 when success, 0 when failed/unstable/aborted.",
		}, []string{jobLabel}),

		LastTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jenkins_build_last_timestamp_seconds",
			Help: "Unix timestamp (seconds) of the latest finished Jenkins build.",
		}, []string{jobLabel}),

		Duration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jenkins_build_duration_seconds",
			Help: "Duration in seconds of the latest finished Jenkins build.",
		}, []string{jobLabel}),

		Total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jenkins_build_total",
			Help: "Total number of Jenkins builds received for this job. Resets to 0 on exporter restart; Prometheus rate() handles counter resets.",
		}, []string{jobLabel}),

		FailureTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jenkins_build_failure_total",
			Help: "Total number of failed Jenkins builds for this job. Resets to 0 on exporter restart; Prometheus rate() handles counter resets.",
		}, []string{jobLabel}),
	}

	for _, c := range []prometheus.Collector{
		m.Status,
		m.LastTimestamp,
		m.Duration,
		m.Total,
		m.FailureTotal,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register build metrics: %w", err)
		}
	}

	return m, nil
}

// DeleteJob 删除某 job 的三个"最近一次构建"Gauge 序列（Task 5 数据过期）。
//
// 刻意不删 Total/FailureTotal 两个 Counter：
//   - 它们是累计统计，删除后重新出现会从 0 重来，破坏 increase()/rate() 的连续性；
//   - 过期只针对"陈旧的状态展示"，历史累计计数仍是有意义的事实。
//
// Prometheus 端 Counter 序列保留但不再更新，表现为一条平线（而非消失/归零）。
func (m *BuildMetrics) DeleteJob(job string) {
	m.Status.DeleteLabelValues(job)
	m.LastTimestamp.DeleteLabelValues(job)
	m.Duration.DeleteLabelValues(job)
}
