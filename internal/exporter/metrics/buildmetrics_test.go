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

func newTestBuildMetrics(t *testing.T) (*BuildMetrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := NewBuildMetrics(reg)
	if err != nil {
		t.Fatalf("NewBuildMetrics: %v", err)
	}
	return m, reg
}

// TestNewBuildMetrics_DuplicateRegister 同一 registry 重复注册必须报错。
func TestNewBuildMetrics_DuplicateRegister(t *testing.T) {
	_, reg := newTestBuildMetrics(t)
	if _, err := NewBuildMetrics(reg); err == nil {
		t.Fatal("expected error on duplicate registration, got nil")
	}
}

// TestBuildMetrics_TypesAndLabels 验证五个指标的类型（3 Gauge + 2 Counter）
// 以及每个指标只有 job_name 一个标签（控制基数，禁止 build_number 入标签）。
func TestBuildMetrics_TypesAndLabels(t *testing.T) {
	m, reg := newTestBuildMetrics(t)

	m.Status.WithLabelValues("pipeline").Set(1)
	m.LastTimestamp.WithLabelValues("pipeline").Set(1700000000)
	m.Duration.WithLabelValues("pipeline").Set(12.5)
	m.Total.WithLabelValues("pipeline").Inc()
	m.FailureTotal.WithLabelValues("pipeline").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	wantTypes := map[string]string{
		"jenkins_build_status":                 "GAUGE",
		"jenkins_build_last_timestamp_seconds": "GAUGE",
		"jenkins_build_duration_seconds":       "GAUGE",
		"jenkins_build_total":                  "COUNTER",
		"jenkins_build_failure_total":          "COUNTER",
	}
	seen := map[string]bool{}
	for _, f := range families {
		name := f.GetName()
		want, ok := wantTypes[name]
		if !ok {
			continue // go_* 等其他指标，跳过
		}
		seen[name] = true
		if got := f.GetType().String(); got != want {
			t.Errorf("%s type = %q, want %q", name, got, want)
		}
		for _, metric := range f.GetMetric() {
			if n := len(metric.GetLabel()); n != 1 {
				t.Errorf("%s has %d labels, want 1", name, n)
			}
			for _, lp := range metric.GetLabel() {
				if lp.GetName() != jobLabel {
					t.Errorf("%s has unexpected label %q", name, lp.GetName())
				}
			}
		}
	}
	for name := range wantTypes {
		if !seen[name] {
			t.Errorf("metric %s not found in gather output", name)
		}
	}
}

// TestBuildMetricsEndpoint 验证 /metrics 文本输出包含 HELP/TYPE 与带标签的样本行，
// 且 jenkins_build_* 与 ops_job_* 能在同一 registry 共存（一次抓取全拿到）。
func TestBuildMetricsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	bm, err := NewBuildMetrics(reg)
	if err != nil {
		t.Fatalf("NewBuildMetrics: %v", err)
	}
	if _, err := NewJobMetrics(reg); err != nil {
		t.Fatalf("NewJobMetrics: %v", err)
	}
	bm.Status.WithLabelValues("pipeline").Set(1)
	bm.Total.WithLabelValues("pipeline").Inc()

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
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, w := range []string{
		"# HELP jenkins_build_status",
		"# TYPE jenkins_build_total counter",
		`jenkins_build_status{job_name="pipeline"} 1`,
		`jenkins_build_total{job_name="pipeline"} 1`,
	} {
		if !strings.Contains(text, w) {
			t.Errorf("metrics output missing %q", w)
		}
	}
}
