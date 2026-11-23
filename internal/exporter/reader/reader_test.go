package reader

import (
	"ops-exporter/internal/exporter/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validTaskJSON 生成一条合法任务结果 JSON 文本。
func validTaskJSON(job, status, start, end string) string {
	return `{
		"job_name": "` + job + `",
		"status": "` + status + `",
		"start_time": "` + start + `",
		"end_time": "` + end + `",
		"exit_code": 0,
		"message": "ok"
	}`
}

// writeFile 在测试临时目录写入文件，失败直接终止测试。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLoadTaskResults_Multiple 验证一个目录下的多个任务都能读到。
func TestLoadTaskResults_Multiple(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "backup.json"),
		validTaskJSON("database_backup", "success", "2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00"))
	writeFile(t, filepath.Join(dir, "sync.json"),
		validTaskJSON("file_sync", "failed", "2026-11-18T02:00:00+08:00", "2026-11-18T02:00:05+08:00"))

	results, fileErrs, err := LoadTaskResults(dir)
	if err != nil {
		t.Fatalf("unexpected dir error: %v", err)
	}
	if len(fileErrs) != 0 {
		t.Fatalf("unexpected file errors: %v", fileErrs)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// os.ReadDir 按文件名排序，所以 backup.json 在 sync.json 之前。
	byName := map[string]model.TaskResult{}
	for _, r := range results {
		byName[r.JobName] = r
	}
	if _, ok := byName["database_backup"]; !ok {
		t.Errorf("missing task database_backup in %v", results)
	}
	// failed 是任务执行失败，仍属于合法数据，必须出现在 results 中。
	if r, ok := byName["file_sync"]; !ok || r.Status != model.StatusFailed {
		t.Errorf("file_sync should be present with status failed, got %+v", r)
	}
}

// TestLoadTaskResults_BadFileIsolated 验证单个坏文件不影响其他任务读取。
func TestLoadTaskResults_BadFileIsolated(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good.json"),
		validTaskJSON("database_backup", "success", "2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00"))
	// 写了一半的 JSON，模拟任务正在写入时被读取。
	writeFile(t, filepath.Join(dir, "half_written.json"), `{"job_name": "log_cleanup", "sta`)
	// 合法 JSON 但缺少必填字段。
	writeFile(t, filepath.Join(dir, "missing_field.json"), `{"job_name":"cron_check","status":"success"}`)

	results, fileErrs, err := LoadTaskResults(dir)
	if err != nil {
		t.Fatalf("dir-level error should be nil, got %v", err)
	}
	if len(results) != 1 || results[0].JobName != "database_backup" {
		t.Fatalf("want only database_backup, got %+v", results)
	}
	if len(fileErrs) != 2 {
		t.Fatalf("got %d file errors, want 2: %v", len(fileErrs), fileErrs)
	}
	// 错误里必须带文件路径，才能定位问题文件。
	for _, fe := range fileErrs {
		if fe.Path == "" {
			t.Errorf("FileError should carry path: %v", fe)
		}
		if fe.Err == nil {
			t.Errorf("FileError should carry cause: %v", fe)
		}
	}
}

// TestLoadTaskResults_SkipNonJSON 验证非 .json 文件和子目录被忽略。
func TestLoadTaskResults_SkipNonJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "backup.json"),
		validTaskJSON("database_backup", "success", "2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00"))
	writeFile(t, filepath.Join(dir, "notes.txt"), "some note")
	writeFile(t, filepath.Join(dir, "run.log"), "log content")
	// 子目录里即使有 json 也不应该被读：第一版只采集一层目录。
	if err := os.Mkdir(filepath.Join(dir, "archive"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "archive", "old.json"),
		validTaskJSON("old_job", "success", "2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00"))

	results, fileErrs, err := LoadTaskResults(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fileErrs) != 0 {
		t.Fatalf("unexpected file errors: %v", fileErrs)
	}
	if len(results) != 1 || results[0].JobName != "database_backup" {
		t.Fatalf("want only database_backup, got %+v", results)
	}
}

// TestLoadTaskResults_UppercaseExt 验证 .JSON 大写扩展名也能被采集。
func TestLoadTaskResults_UppercaseExt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "BACKUP.JSON"),
		validTaskJSON("database_backup", "success", "2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00"))

	results, fileErrs, err := LoadTaskResults(dir)
	if err != nil || len(fileErrs) != 0 || len(results) != 1 {
		t.Fatalf("want 1 result, 0 errors; got results=%v fileErrs=%v err=%v", results, fileErrs, err)
	}
}

// TestLoadTaskResults_EmptyDir 空目录不是错误，只是没有任务。
func TestLoadTaskResults_EmptyDir(t *testing.T) {
	results, fileErrs, err := LoadTaskResults(t.TempDir())
	if err != nil {
		t.Fatalf("empty dir should not be an error: %v", err)
	}
	if len(results) != 0 || len(fileErrs) != 0 {
		t.Fatalf("want empty result, got results=%v fileErrs=%v", results, fileErrs)
	}
}

// TestLoadTaskResults_DirNotExist 目录不存在属于目录级失败，
// 和"单个文件读不出来"必须区分开。
func TestLoadTaskResults_DirNotExist(t *testing.T) {
	_, _, err := LoadTaskResults(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected dir-level error, got nil")
	}
}

// TestLoadTaskResults_EmptyFile 空文件（0 字节）是真实场景：
// 任务方创建文件后尚未写入内容就被 exporter 读取。
// 必须作为文件级错误处理，不能崩溃也不能静默跳过。
func TestLoadTaskResults_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "empty.json"), "")
	writeFile(t, filepath.Join(dir, "good.json"),
		validTaskJSON("database_backup", "success", "2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00"))

	results, fileErrs, err := LoadTaskResults(dir)
	if err != nil {
		t.Fatalf("unexpected dir error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if len(fileErrs) != 1 || !strings.Contains(fileErrs[0].Err.Error(), "unexpected end of JSON input") {
		t.Errorf("want empty file reported as parse error, got %v", fileErrs)
	}
}
