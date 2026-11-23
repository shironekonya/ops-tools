package refresher

// 本文件实现 TODO 7 的验收语义：
// 通过 ops_job_last_run_timestamp_seconds + PromQL 识别超时未执行的任务。
// 代码层面要锁死两条约定，保证 PromQL 不漏报：
//  1. 任务文件消失后，指标序列保留最后一次的值（否则 time() 查询失效）
//  2. exporter 不为没见过的任务伪造"正常"数据；自身刷新新鲜度独立暴露

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLastRefreshGauge 验证 exporter 自身新鲜度指标：
// 刷新成功后等于当前时间（5 秒容差内）。
func TestLastRefreshGauge(t *testing.T) {
	m, reg := newTestMetrics(t)
	dir := t.TempDir()
	writeTaskFile(t, dir, "backup.json", "database_backup", "success", time.Now())

	if err := refreshOnce(dir, m, newRefreshTracker()); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	got, found := gaugeNoLabels(t, reg, "ops_exporter_last_refresh_timestamp_seconds")
	if !found {
		t.Fatal("ops_exporter_last_refresh_timestamp_seconds not exposed")
	}
	if diff := time.Since(time.Unix(int64(got), 0)); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("last_refresh = %v, too far from now", got)
	}
}

// TestStaleSeriesPersistAfterFileRemoval 是超时检测能工作的关键前提：
// 任务文件被删除（比如任务下线或任务方不再写入）后，
// 指标序列必须保留最后一次执行的数据。
// 这样 time() - ops_job_last_run_timestamp_seconds 才会随时间持续增长，
// 触发"超时未执行"告警；如果序列被清理，查询会静默失效（漏报）。
func TestStaleSeriesPersistAfterFileRemoval(t *testing.T) {
	m, reg := newTestMetrics(t)
	dir := t.TempDir()

	oldEnd := time.Now().Add(-48 * time.Hour)
	writeTaskFile(t, dir, "backup.json", "database_backup", "success", oldEnd)
	if err := refreshOnce(dir, m, newRefreshTracker()); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	// 模拟任务方不再写入：删除文件后再刷新若干轮
	if err := os.Remove(filepath.Join(dir, "backup.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := refreshOnce(dir, m, newRefreshTracker()); err != nil {
			t.Fatalf("refreshOnce: %v", err)
		}
	}

	v, ok := gauge(t, reg, "ops_job_last_run_timestamp_seconds", "database_backup")
	if !ok {
		t.Fatal("series disappeared after file removal; time()-based staleness alerts would go silent")
	}
	if v != float64(oldEnd.Unix()) {
		t.Errorf("last_run = %v, want preserved %v", v, float64(oldEnd.Unix()))
	}
}

// TestUnknownJobNeverFaked 验收"不把从未执行过的任务误判为正常"的 exporter 侧：
// exporter 对没见过的任务不伪造任何值（既不写 0 也不写当前时间），
// 避免 PromQL 拿到看似正常的数据。
// 完全没登记过的任务由告警规则层用 absent() 处理（见 promql.md）。
func TestUnknownJobNeverFaked(t *testing.T) {
	m, reg := newTestMetrics(t)
	dir := t.TempDir()
	writeTaskFile(t, dir, "backup.json", "database_backup", "success", time.Now())

	if err := refreshOnce(dir, m, newRefreshTracker()); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	// 从未出现过的任务不应该有任何序列
	if _, ok := gauge(t, reg, "ops_job_last_run_timestamp_seconds", "ghost_job"); ok {
		t.Error("unknown job ghost_job must not have a fabricated series")
	}
	if _, ok := gauge(t, reg, "ops_job_last_status", "ghost_job"); ok {
		t.Error("unknown job ghost_job must not have a fabricated status series")
	}
}

// TestLastRefreshStopsOnDirFailure TODO 7 的关键语义补测：
// 目录级刷新失败时，ops_exporter_last_refresh_timestamp_seconds 必须停止前进，
// 这样 time() - 它 才能发现 exporter 自身失效（否则永远显示"刚刷新过"）。
func TestLastRefreshStopsOnDirFailure(t *testing.T) {
	m, reg := newTestMetrics(t)
	dir := t.TempDir()
	writeTaskFile(t, dir, "backup.json", "database_backup", "success", time.Now())

	// 第一轮成功：last_refresh 有值
	if err := refreshOnce(dir, m, newRefreshTracker()); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	first, ok := gaugeNoLabels(t, reg, "ops_exporter_last_refresh_timestamp_seconds")
	if !ok {
		t.Fatal("last_refresh not set after successful refresh")
	}

	// 目录消失后刷新失败：last_refresh 必须保持原值
	missing := filepath.Join(dir, "gone")
	if err := refreshOnce(missing, m, newRefreshTracker()); err == nil {
		t.Fatal("expected error for missing dir")
	}
	after, ok := gaugeNoLabels(t, reg, "ops_exporter_last_refresh_timestamp_seconds")
	if !ok {
		t.Fatal("last_refresh series disappeared")
	}
	if after != first {
		t.Errorf("last_refresh advanced on failed refresh: %v -> %v, want unchanged", first, after)
	}
}
