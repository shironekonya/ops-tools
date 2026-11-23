package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// metricValue 从 Gather 结果里取某指标某 job 的样本值。
func metricValue(t *testing.T, reg *prometheus.Registry, name, job string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == jobLabel && lp.GetValue() == job {
					if f.GetType().String() == "COUNTER" {
						return metric.GetCounter().GetValue()
					}
					return metric.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("no series for %s{job_name=%q}", name, job)
	return 0
}

// TestBuildMetricsUpdate_StatusMapping 四种状态的 status 值：
// success→1，其余（failed/unstable/aborted）→0。
func TestBuildMetricsUpdate_StatusMapping(t *testing.T) {
	cases := []struct {
		status string
		want   float64
	}{
		{BuildStatusSuccess, 1},
		{BuildStatusFailed, 0},
		{BuildStatusUnstable, 0},
		{BuildStatusAborted, 0},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			m, reg := newTestBuildMetrics(t)
			m.Update(BuildResult{
				JobName: "pipeline", Status: tc.status,
				BuildNumber: 1, Duration: 10, Timestamp: sampleTimestamp,
			})
			if got := metricValue(t, reg, "jenkins_build_status", "pipeline"); got != tc.want {
				t.Errorf("status = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildMetricsUpdate_Gauges 验证 timestamp/duration 正确写入指标。
func TestBuildMetricsUpdate_Gauges(t *testing.T) {
	m, reg := newTestBuildMetrics(t)
	m.Update(BuildResult{
		JobName: "pipeline", Status: BuildStatusSuccess,
		BuildNumber: 1, Duration: 123.5, Timestamp: sampleTimestamp,
	})

	if got := metricValue(t, reg, "jenkins_build_last_timestamp_seconds", "pipeline"); got != float64(sampleTimestamp) {
		t.Errorf("last_timestamp = %v, want %d", got, sampleTimestamp)
	}
	if got := metricValue(t, reg, "jenkins_build_duration_seconds", "pipeline"); got != 123.5 {
		t.Errorf("duration = %v, want 123.5", got)
	}
}

// TestBuildMetricsUpdate_Counters 验证 Counter 累加规则：
// 每 accepted 一次 total +1，只有 failed 时 failure_total +1。
func TestBuildMetricsUpdate_Counters(t *testing.T) {
	m, reg := newTestBuildMetrics(t)
	builds := []BuildResult{
		{JobName: "pipeline", Status: BuildStatusSuccess, BuildNumber: 1, Timestamp: 100},
		{JobName: "pipeline", Status: BuildStatusFailed, BuildNumber: 2, Timestamp: 200},
		{JobName: "pipeline", Status: BuildStatusUnstable, BuildNumber: 3, Timestamp: 300},
		{JobName: "pipeline", Status: BuildStatusAborted, BuildNumber: 4, Timestamp: 400},
		{JobName: "pipeline", Status: BuildStatusFailed, BuildNumber: 5, Timestamp: 500},
	}
	for _, b := range builds {
		m.Update(b)
	}

	if got := metricValue(t, reg, "jenkins_build_total", "pipeline"); got != 5 {
		t.Errorf("total = %v, want 5", got)
	}
	if got := metricValue(t, reg, "jenkins_build_failure_total", "pipeline"); got != 2 {
		t.Errorf("failure_total = %v, want 2 (only failed, not unstable/aborted)", got)
	}
}

// TestBuildMetricsUpdate_MultipleJobs 不同 job 的序列相互独立，Counter 各自计数。
func TestBuildMetricsUpdate_MultipleJobs(t *testing.T) {
	m, reg := newTestBuildMetrics(t)
	m.Update(BuildResult{JobName: "job-a", Status: BuildStatusSuccess, BuildNumber: 1, Timestamp: 100})
	m.Update(BuildResult{JobName: "job-a", Status: BuildStatusSuccess, BuildNumber: 2, Timestamp: 200})
	m.Update(BuildResult{JobName: "job-b", Status: BuildStatusFailed, BuildNumber: 1, Timestamp: 300})

	if got := metricValue(t, reg, "jenkins_build_total", "job-a"); got != 2 {
		t.Errorf("job-a total = %v, want 2", got)
	}
	if got := metricValue(t, reg, "jenkins_build_total", "job-b"); got != 1 {
		t.Errorf("job-b total = %v, want 1", got)
	}
	if got := metricValue(t, reg, "jenkins_build_failure_total", "job-a"); got != 0 {
		t.Errorf("job-a failure_total = %v, want 0", got)
	}
	if got := metricValue(t, reg, "jenkins_build_failure_total", "job-b"); got != 1 {
		t.Errorf("job-b failure_total = %v, want 1", got)
	}
}

// TestBuildMetricsUpdate_LatestWins Gauge 随新 Build 更新（监测"最近一次"语义）。
func TestBuildMetricsUpdate_LatestWins(t *testing.T) {
	m, reg := newTestBuildMetrics(t)
	m.Update(BuildResult{JobName: "p", Status: BuildStatusSuccess, BuildNumber: 1, Duration: 10, Timestamp: 100})
	m.Update(BuildResult{JobName: "p", Status: BuildStatusFailed, BuildNumber: 2, Duration: 20, Timestamp: 200})

	if got := metricValue(t, reg, "jenkins_build_status", "p"); got != 0 {
		t.Errorf("status = %v, want 0 (latest failed)", got)
	}
	if got := metricValue(t, reg, "jenkins_build_duration_seconds", "p"); got != 20 {
		t.Errorf("duration = %v, want 20 (latest)", got)
	}
}
