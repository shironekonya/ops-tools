package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newTestMetrics 为测试创建一个独立 registry 和一套指标，
// 避免不同测试之间互相影响（同一个 registry 重复注册会报错）。
func newTestMetrics(t *testing.T) (*JobMetrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := NewJobMetrics(reg)
	if err != nil {
		t.Fatalf("NewJobMetrics: %v", err)
	}
	return m, reg
}

// TestNewJobMetrics_DuplicateRegister 验证同一 registry 上重复注册会被拒绝，
// 而不是静默产生两套同名指标。
func TestNewJobMetrics_DuplicateRegister(t *testing.T) {
	_, reg := newTestMetrics(t)

	if _, err := NewJobMetrics(reg); err == nil {
		t.Fatal("expected error on duplicate registration, got nil")
	}
}

// TestJobMetrics_SeparateSeriesPerJob 验证不同任务拥有相互独立的时间序列，
// 且每个指标的标签只有 job_name。
func TestJobMetrics_SeparateSeriesPerJob(t *testing.T) {
	m, reg := newTestMetrics(t)

	// 模拟两个任务：一个成功、一个失败（失败不代表读取失败，仍要更新指标）。
	m.LastStatus.WithLabelValues("database_backup").Set(1)
	m.LastRunTimestamp.WithLabelValues("database_backup").Set(1763424330)
	m.LastSuccessTimestamp.WithLabelValues("database_backup").Set(1763424330)
	m.LastDuration.WithLabelValues("database_backup").Set(330)

	m.LastStatus.WithLabelValues("file_sync").Set(0)
	m.LastRunTimestamp.WithLabelValues("file_sync").Set(1763425000)
	// 该任务从未成功过：按约定显式写 0，而不是留空或沿用旧值。
	m.LastSuccessTimestamp.WithLabelValues("file_sync").Set(0)
	m.LastDuration.WithLabelValues("file_sync").Set(5)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// 收集成 map 方便断言：family -> job -> value
	got := map[string]map[string]float64{}
	types := map[string]string{}
	for _, f := range families {
		types[f.GetName()] = f.GetType().String()
		for _, metric := range f.GetMetric() {
			var job string
			for _, lp := range metric.GetLabel() {
				if lp.GetName() != jobLabel {
					t.Errorf("%s has unexpected label %q", f.GetName(), lp.GetName())
				}
				job = lp.GetValue()
			}
			if got[f.GetName()] == nil {
				got[f.GetName()] = map[string]float64{}
			}
			got[f.GetName()][job] = metric.GetGauge().GetValue()
		}
	}

	cases := []struct {
		metric string
		job    string
		want   float64
	}{
		{"ops_job_last_status", "database_backup", 1},
		{"ops_job_last_status", "file_sync", 0},
		{"ops_job_last_run_timestamp_seconds", "database_backup", 1763424330},
		{"ops_job_last_success_timestamp_seconds", "database_backup", 1763424330},
		{"ops_job_last_success_timestamp_seconds", "file_sync", 0},
		{"ops_job_last_duration_seconds", "database_backup", 330},
		{"ops_job_last_duration_seconds", "file_sync", 5},
	}
	for _, c := range cases {
		if types[c.metric] != "GAUGE" {
			t.Errorf("%s type = %q, want GAUGE", c.metric, types[c.metric])
		}
		v, ok := got[c.metric][c.job]
		if !ok {
			t.Errorf("%s has no series for job %q", c.metric, c.job)
			continue
		}
		if v != c.want {
			t.Errorf("%s{job_name=%q} = %v, want %v", c.metric, c.job, v, c.want)
		}
	}
}

// TestMetricsEndpoint 验证 /metrics 输出的文本内容符合 Prometheus 格式，
// 包括 HELP、TYPE 和带标签的样本行。
func TestMetricsEndpoint(t *testing.T) {
	m, reg := newTestMetrics(t)
	m.LastStatus.WithLabelValues("database_backup").Set(1)
	m.LastStatus.WithLabelValues("file_sync").Set(0)

	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	wants := []string{
		"# HELP ops_job_last_status",
		"# TYPE ops_job_last_status gauge",
		`ops_job_last_status{job_name="database_backup"} 1`,
		`ops_job_last_status{job_name="file_sync"} 0`,
	}
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("metrics output missing %q", w)
		}
	}
}
