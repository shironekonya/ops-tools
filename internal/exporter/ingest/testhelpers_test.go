package ingest

import (
	"testing"

	"ops-exporter/internal/exporter/model"

	"github.com/prometheus/client_golang/prometheus"
)

// 拆包前这些 helper/常量定义在 build_test.go 与 buildupdater_test.go（同 package main），
// 拆包后用同包测试文件补回，让 ingest 测试代码零改动。
const BuildStatusSuccess = model.BuildStatusSuccess

const sampleTimestamp int64 = 1700000000

func validBuildJSON() string {
	return `{
		"job_name": "my-pipeline",
		"build_number": 42,
		"status": "failed",
		"duration": 123.5,
		"timestamp": 1700000000,
		"failed_stage": "deploy"
	}`
}

// metricValue 从 Gather 结果里取某指标某 job 的样本值（支持 Gauge 与 Counter）。
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
				if lp.GetName() == "job_name" && lp.GetValue() == job {
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
