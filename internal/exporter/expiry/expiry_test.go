package expiry

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"ops-exporter/internal/exporter/metrics"
	"ops-exporter/internal/exporter/model"
	"ops-exporter/internal/exporter/store"
)

const jobLabelName = "job_name"

// seriesExists 判断某个指标+job 标签的序列是否还在 registry 中。
func seriesExists(t *testing.T, reg *prometheus.Registry, name, job string) bool {
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
				if lp.GetName() == jobLabelName && lp.GetValue() == job {
					return true
				}
			}
		}
	}
	return false
}

// TestExpirer_PruneOnce 核心：过期 job 的三个 Gauge 序列被删除，
// 两个 Counter 序列保留（累计值不因过期而丢失），未过期 job 不受影响。
func TestExpirer_PruneOnce(t *testing.T) {
	reg := prometheus.NewRegistry()
	bm, err := metrics.NewBuildMetrics(reg)
	if err != nil {
		t.Fatalf("NewBuildMetrics: %v", err)
	}
	s := &store.BuildStore{}

	base := int64(1700000000)
	stale := model.BuildResult{JobName: "stale-job", BuildNumber: 1, Status: model.BuildStatusFailed, Timestamp: base - 7200, Duration: 10} // 2 小时前
	fresh := model.BuildResult{JobName: "fresh-job", BuildNumber: 1, Status: model.BuildStatusSuccess, Timestamp: base, Duration: 5}        // 当前
	s.Save(stale)
	s.Save(fresh)
	bm.Update(stale)
	bm.Update(fresh)

	e := New(s, bm, time.Hour, slog.Default())
	removed := e.PruneOnce(time.Unix(base, 0)) // cutoff = base-3600，stale(base-7200) 过期，fresh(base) 保留
	if removed != 1 {
		t.Fatalf("PruneOnce removed = %d, want 1", removed)
	}

	// 过期 job：Gauge 消失，Counter 保留。
	for _, gauge := range []string{"jenkins_build_status", "jenkins_build_last_timestamp_seconds", "jenkins_build_duration_seconds"} {
		if seriesExists(t, reg, gauge, "stale-job") {
			t.Errorf("stale job gauge %s should be deleted", gauge)
		}
	}
	for _, counter := range []string{"jenkins_build_total", "jenkins_build_failure_total"} {
		if !seriesExists(t, reg, counter, "stale-job") {
			t.Errorf("stale job counter %s should be preserved", counter)
		}
	}

	// 未过期 job：全部序列保留。
	for _, name := range []string{
		"jenkins_build_status", "jenkins_build_last_timestamp_seconds", "jenkins_build_duration_seconds",
		"jenkins_build_total", "jenkins_build_failure_total",
	} {
		if !seriesExists(t, reg, name, "fresh-job") {
			t.Errorf("fresh job series %s should remain", name)
		}
	}
}

// TestExpirer_Disabled maxAge<=0 时 Run 立即返回，不启动 ticker、不删除任何数据。
func TestExpirer_Disabled(t *testing.T) {
	reg := prometheus.NewRegistry()
	bm, _ := metrics.NewBuildMetrics(reg)
	s := &store.BuildStore{}
	oldBuild := model.BuildResult{JobName: "old", BuildNumber: 1, Status: model.BuildStatusSuccess, Timestamp: 1}
	s.Save(oldBuild)
	bm.Update(oldBuild) // 先写入序列，才能验证"禁用时序列不被删"

	e := New(s, bm, 0, slog.Default())
	done := make(chan struct{})
	go func() {
		e.Run(context.Background(), time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled expirer should return immediately")
	}
	if !seriesExists(t, reg, "jenkins_build_status", "old") {
		t.Error("disabled expirer must not delete series")
	}
}

// TestExpirer_RunCancellation 启用时按 interval 清理，ctx 取消后退出。
func TestExpirer_RunCancellation(t *testing.T) {
	reg := prometheus.NewRegistry()
	bm, _ := metrics.NewBuildMetrics(reg)
	s := &store.BuildStore{}
	base := int64(1700000000)
	s.Save(model.BuildResult{JobName: "old", BuildNumber: 1, Status: model.BuildStatusFailed, Timestamp: base})
	bm.Update(model.BuildResult{JobName: "old", BuildNumber: 1, Status: model.BuildStatusFailed, Timestamp: base})

	e := New(s, bm, time.Second, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx, 20*time.Millisecond)
		close(done)
	}()

	// 立即清理一次（Run 先 PruneOnce），old 的时间戳 base 远早于 now，应被删除。
	deadline := time.After(time.Second)
	for {
		if !seriesExists(t, reg, "jenkins_build_status", "old") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expirer did not remove stale series")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expirer did not stop on ctx cancellation")
	}
}
