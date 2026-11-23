package metrics

import (
	"ops-exporter/internal/exporter/model"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 测试用的固定时间，避免测试结果随当前时间漂移。
var (
	tStart = time.Date(2026, 11, 19, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	tEnd   = tStart.Add(5*time.Minute + 30*time.Second) // 330 秒
	tEnd2  = tStart.Add(10 * time.Minute)               // 600 秒
	tsEnd  = float64(tEnd.Unix())
	tsEnd2 = float64(tEnd2.Unix())
)

// task 构造一条任务结果。
func task(job, status string, start, end time.Time) model.TaskResult {
	return model.TaskResult{
		JobName:   job,
		Status:    status,
		StartTime: start,
		EndTime:   end,
		ExitCode:  0,
	}
}

// gauge 读出指定指标、指定 job 的当前值。
// notExist 表示该序列不应存在。
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

// TestUpdate_SuccessTask 验证成功任务的四个指标全部符合预期。
func TestUpdate_SuccessTask(t *testing.T) {
	m, reg := newTestMetrics(t)
	m.Update(task("database_backup", model.StatusSuccess, tStart, tEnd))

	if v, _ := gauge(t, reg, "ops_job_last_status", "database_backup"); v != 1 {
		t.Errorf("last_status = %v, want 1", v)
	}
	if v, _ := gauge(t, reg, "ops_job_last_run_timestamp_seconds", "database_backup"); v != tsEnd {
		t.Errorf("last_run = %v, want %v", v, tsEnd)
	}
	if v, _ := gauge(t, reg, "ops_job_last_success_timestamp_seconds", "database_backup"); v != tsEnd {
		t.Errorf("last_success = %v, want %v", v, tsEnd)
	}
	if v, _ := gauge(t, reg, "ops_job_last_duration_seconds", "database_backup"); v != 330 {
		t.Errorf("last_duration = %v, want 330", v)
	}
}

// TestUpdate_FailedThenSuccess 验证"先失败后成功"：
// 失败时 last_success 首次出现且为 0，成功后写入正确时间。
func TestUpdate_FailedThenSuccess(t *testing.T) {
	m, reg := newTestMetrics(t)

	// 第一次执行失败，从未成功过
	m.Update(task("file_sync", model.StatusFailed, tStart, tEnd))
	if v, ok := gauge(t, reg, "ops_job_last_success_timestamp_seconds", "file_sync"); !ok || v != 0 {
		t.Errorf("first failed run: last_success = %v (exist=%v), want 0", v, ok)
	}
	if v, _ := gauge(t, reg, "ops_job_last_status", "file_sync"); v != 0 {
		t.Errorf("last_status = %v, want 0", v)
	}

	// 第二次执行成功，成功时间被写入
	m.Update(task("file_sync", model.StatusSuccess, tEnd, tEnd2))
	if v, _ := gauge(t, reg, "ops_job_last_success_timestamp_seconds", "file_sync"); v != tsEnd2 {
		t.Errorf("after success: last_success = %v, want %v", v, tsEnd2)
	}
	if v, _ := gauge(t, reg, "ops_job_last_status", "file_sync"); v != 1 {
		t.Errorf("last_status = %v, want 1", v)
	}
}

// TestUpdate_SuccessThenFailed 是本 TODO 的核心验收点：
// 失败任务绝不能覆盖最近成功时间。
func TestUpdate_SuccessThenFailed(t *testing.T) {
	m, reg := newTestMetrics(t)

	m.Update(task("database_backup", model.StatusSuccess, tStart, tEnd))
	m.Update(task("database_backup", model.StatusFailed, tEnd, tEnd2))

	if v, _ := gauge(t, reg, "ops_job_last_status", "database_backup"); v != 0 {
		t.Errorf("last_status = %v, want 0 (failed)", v)
	}
	// last_run / duration 跟随最近一次（失败的）执行
	if v, _ := gauge(t, reg, "ops_job_last_run_timestamp_seconds", "database_backup"); v != tsEnd2 {
		t.Errorf("last_run = %v, want %v", v, tsEnd2)
	}
	if v, _ := gauge(t, reg, "ops_job_last_duration_seconds", "database_backup"); v != 270 {
		t.Errorf("last_duration = %v, want 270", v)
	}
	// last_success 保留成功那次的时间，不被失败覆盖
	if v, _ := gauge(t, reg, "ops_job_last_success_timestamp_seconds", "database_backup"); v != tsEnd {
		t.Errorf("last_success = %v, want %v (must not be overwritten by failure)", v, tsEnd)
	}
}

// TestApplyResults_DuplicateJob 验证同一 job_name 多条结果时采纳 end_time 最新的。
func TestApplyResults_DuplicateJob(t *testing.T) {
	m, reg := newTestMetrics(t)

	older := task("log_cleanup", model.StatusFailed, tStart, tEnd)
	newer := task("log_cleanup", model.StatusSuccess, tEnd, tEnd2)
	// 顺序故意颠倒，去重不能依赖输入顺序。
	m.ApplyResults([]model.TaskResult{newer, older})

	if v, _ := gauge(t, reg, "ops_job_last_status", "log_cleanup"); v != 1 {
		t.Errorf("last_status = %v, want 1 (newer result is success)", v)
	}
	if v, _ := gauge(t, reg, "ops_job_last_run_timestamp_seconds", "log_cleanup"); v != tsEnd2 {
		t.Errorf("last_run = %v, want %v", v, tsEnd2)
	}
}

// TestApplyResults_MultipleJobs 验证批量入口下各任务互不干扰。
func TestApplyResults_MultipleJobs(t *testing.T) {
	m, reg := newTestMetrics(t)

	m.ApplyResults([]model.TaskResult{
		task("database_backup", model.StatusSuccess, tStart, tEnd),
		task("file_sync", model.StatusFailed, tStart, tEnd2),
	})

	if v, _ := gauge(t, reg, "ops_job_last_status", "database_backup"); v != 1 {
		t.Errorf("database_backup last_status = %v, want 1", v)
	}
	if v, _ := gauge(t, reg, "ops_job_last_status", "file_sync"); v != 0 {
		t.Errorf("file_sync last_status = %v, want 0", v)
	}
	// 成功与失败互不影响对方的 last_success
	if v, _ := gauge(t, reg, "ops_job_last_success_timestamp_seconds", "database_backup"); v != tsEnd {
		t.Errorf("database_backup last_success = %v, want %v", v, tsEnd)
	}
	if v, _ := gauge(t, reg, "ops_job_last_success_timestamp_seconds", "file_sync"); v != 0 {
		t.Errorf("file_sync last_success = %v, want 0 (never succeeded)", v)
	}
}

// TestUpdate_LabelCardinality 验证任务指标（ops_job_*）的标签始终只有
// job_name 一个，message / 路径 / 时间戳不允许混进来。
// exporter 自身指标（ops_exporter_*）不带 job 标签，不在本测试范围。
func TestUpdate_LabelCardinality(t *testing.T) {
	m, reg := newTestMetrics(t)
	r := task("database_backup", model.StatusSuccess, tStart, tEnd)
	r.Message = "backup completed with 128 tables"
	m.Update(r)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "ops_job_") {
			continue // 只约束任务指标
		}
		for _, metric := range f.GetMetric() {
			if n := len(metric.GetLabel()); n != 1 {
				t.Errorf("%s has %d labels, want exactly 1 (job_name)", f.GetName(), n)
			}
		}
	}
}

// TestApplyResults_EmptyBatch 空批量（目录里没有任何合法任务）：
// 不 panic、不产生任何序列，已有序列不受影响。
func TestApplyResults_EmptyBatch(t *testing.T) {
	m, reg := newTestMetrics(t)

	// 先放一个任务进去
	m.ApplyResults([]model.TaskResult{task("database_backup", model.StatusSuccess, tStart, tEnd)})
	// 空批量刷新
	m.ApplyResults(nil)

	if v, ok := gauge(t, reg, "ops_job_last_status", "database_backup"); !ok || v != 1 {
		t.Errorf("existing series must survive empty batch, got %v (exist=%v)", v, ok)
	}
	// 不应出现任何未知任务序列
	if _, ok := gauge(t, reg, "ops_job_last_status", "ghost"); ok {
		t.Error("empty batch must not create series")
	}
}
