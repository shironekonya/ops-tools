package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// jobLabel 是所有运维任务指标共用的唯一标签名。
//
// 只用 job_name 作为标签是刻意的：
//   - message 内容不可控且几乎每次都不同，作为标签会导致时间序列爆炸
//   - 文件路径属于实现细节，会随部署变化
//   - 时间戳应当放在指标值里，而不是标签里
//
// 标签取值基数等于任务数量，这是可控的。
const jobLabel = "job_name"

// JobMetrics 保存 Ops Exporter 暴露的全部指标。
//
// 四个指标都是 Gauge 而不是 Counter：
// 它们描述"最近一次执行的状态"，会随任务新一轮执行而上升或下降
// （例如失败时 ops_job_last_status 从 1 变成 0），
// 而 Counter 只能单调递增，语义不符。
//
// 用 GaugeVec 是因为同一个指标要按 job_name 分成多条时间序列，
// 每条序列由 WithLabelValues(jobName) 单独定位。
type JobMetrics struct {
	// LastStatus 最近一次任务状态：成功 1，失败 0。
	LastStatus *prometheus.GaugeVec

	// LastRunTimestamp 最近一次任务结束时间（Unix 秒）。
	// PromQL 用 time() - 该值 判断任务多久没跑了。
	LastRunTimestamp *prometheus.GaugeVec

	// LastSuccessTimestamp 最近一次成功结束时间（Unix 秒）。
	// 失败的任务不允许覆盖它，否则会掩盖"一直失败"的问题。
	// 从未成功过的任务显式设为 0，便于用 == 0 或 time()-值 识别。
	LastSuccessTimestamp *prometheus.GaugeVec

	// LastDuration 最近一次执行耗时（秒），等于 end_time - start_time。
	LastDuration *prometheus.GaugeVec

	// LastRefresh exporter 最近一次成功刷新任务结果的时间（Unix 秒）。
	// 不带 job 标签，描述 exporter 自身而非某个任务。
	// 用途：区分"任务长时间没跑"和"exporter 自己没在刷新"——
	// 后者会让所有 time() - ops_job_* 类查询同时失真，必须能独立发现。
	// 刷新失败（如目录读不了）时不更新它，让它自然变"陈旧"。
	LastRefresh prometheus.Gauge
}

// NewJobMetrics 创建四个指标并注册到给定 registry。
//
// 注册失败会立刻返回错误而不是忽略：
// 重复注册同名指标会让 /metrics 输出异常，
// 属于编程错误，必须在启动阶段暴露。
//
// 传入 registry 而不是使用 prometheus.DefaultRegisterer，
// 是为了让测试能用独立 registry，避免多次注册冲突。
func NewJobMetrics(reg prometheus.Registerer) (*JobMetrics, error) {
	m := &JobMetrics{
		LastStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ops_job_last_status",
			Help: "Last execution status of the ops job: 1 when success, 0 when failed.",
		}, []string{jobLabel}),

		LastRunTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ops_job_last_run_timestamp_seconds",
			Help: "Unix timestamp (seconds) of the last finished execution of the ops job.",
		}, []string{jobLabel}),

		LastSuccessTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ops_job_last_success_timestamp_seconds",
			Help: "Unix timestamp (seconds) of the last successful execution of the ops job, 0 if it never succeeded.",
		}, []string{jobLabel}),

		LastDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ops_job_last_duration_seconds",
			Help: "Duration in seconds of the last finished execution of the ops job.",
		}, []string{jobLabel}),

		LastRefresh: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ops_exporter_last_refresh_timestamp_seconds",
			Help: "Unix timestamp (seconds) of the last successful task result refresh by this exporter.",
		}),
	}

	for _, c := range []prometheus.Collector{
		m.LastStatus,
		m.LastRunTimestamp,
		m.LastSuccessTimestamp,
		m.LastDuration,
		m.LastRefresh,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("register metrics: %w", err)
		}
	}

	return m, nil
}
