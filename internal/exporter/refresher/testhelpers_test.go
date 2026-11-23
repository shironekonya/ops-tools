package refresher

import (
	"testing"

	"ops-exporter/internal/exporter/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

// 拆包前这些 helper 定义在 metrics_test.go / updater_test.go / staleness_test.go（同 package main），
// 拆包后用同包测试文件补回，让 refresher 测试代码零改动。

const jobLabel = "job_name"

func newTestMetrics(t *testing.T) (*metrics.JobMetrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := metrics.NewJobMetrics(reg)
	if err != nil {
		t.Fatalf("NewJobMetrics: %v", err)
	}
	return m, reg
}

// gauge 读指定指标、指定 job 的 Gauge 值。
func gauge(t *testing.T, reg *prometheus.Registry, name, job string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == jobLabel && lp.GetValue() == job {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// gaugeNoLabels 读无标签 Gauge 的值。
func gaugeNoLabels(t *testing.T, reg *prometheus.Registry, name string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) != 0 {
				t.Errorf("%s should have no labels", name)
				continue
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}
