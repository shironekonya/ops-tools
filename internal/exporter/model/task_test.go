package model

import (
	"strings"
	"testing"
	"time"
)

// TestParseTaskResult_Valid 验证合法 JSON 能被正确解析。
func TestParseTaskResult_Valid(t *testing.T) {
	data := []byte(`{
		"job_name": "database_backup",
		"status": "success",
		"start_time": "2026-11-18T01:00:00+08:00",
		"end_time": "2026-11-18T01:05:30+08:00",
		"exit_code": 0,
		"message": "backup completed"
	}`)

	r, err := ParseTaskResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.JobName != "database_backup" {
		t.Errorf("JobName = %q, want %q", r.JobName, "database_backup")
	}
	if r.Status != StatusSuccess {
		t.Errorf("Status = %q, want %q", r.Status, StatusSuccess)
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", r.ExitCode)
	}
	if r.Message != "backup completed" {
		t.Errorf("Message = %q, want %q", r.Message, "backup completed")
	}
	wantStart, _ := time.Parse(time.RFC3339, "2026-11-18T01:00:00+08:00")
	if !r.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %s, want %s", r.StartTime, wantStart)
	}
}

// TestParseTaskResult_FailedTask 验证 failed 状态的任务同样合法。
func TestParseTaskResult_FailedTask(t *testing.T) {
	data := []byte(`{
		"job_name": "log_cleanup",
		"status": "failed",
		"start_time": "2026-11-18T03:00:00+08:00",
		"end_time": "2026-11-18T03:00:10+08:00",
		"exit_code": 1
	}`)
	r, err := ParseTaskResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != StatusFailed {
		t.Errorf("Status = %q, want %q", r.Status, StatusFailed)
	}
	// message 选填，缺省应为空字符串
	if r.Message != "" {
		t.Errorf("Message = %q, want empty", r.Message)
	}
}

// TestParseTaskResult_Invalid 集中覆盖各种非法输入：
// 每种情况都必须返回 error，且不允许 panic。
func TestParseTaskResult_Invalid(t *testing.T) {
	valid := func(job, status, start, end string, extra string) string {
		return `{"job_name":"` + job + `","status":"` + status +
			`","start_time":"` + start + `","end_time":"` + end + `","exit_code":0` + extra + `}`
	}
	base := valid("database_backup", "success",
		"2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00", "")

	cases := []struct {
		name    string
		data    string
		wantErr string // 期望错误信息包含的片段
	}{
		{"JSON 语法错误", `{"job_name": "x", `, "invalid task result JSON"},
		{"完全不是 JSON", `not a json`, "invalid task result JSON"},
		{"任务名为空", strings.Replace(base, `"database_backup"`, `""`, 1), "job_name"},
		{"缺少任务名", `{"status":"success","start_time":"2026-11-18T01:00:00+08:00","end_time":"2026-11-18T01:05:30+08:00"}`, "job_name"},
		{"状态值非法", valid("db", "running", "2026-11-18T01:00:00+08:00", "2026-11-18T01:05:30+08:00", ""), "invalid status"},
		{"缺少状态", strings.Replace(base, `"status":"success",`, ``, 1), "invalid status"},
		{"开始时间格式错误", valid("db", "success", "2026/11/18 01:00", "2026-11-18T01:05:30+08:00", ""), "invalid task result JSON"},
		{"结束时间格式错误", valid("db", "success", "2026-11-18T01:00:00+08:00", "yesterday", ""), "invalid task result JSON"},
		{"缺少开始时间", `{"job_name":"db","status":"success","end_time":"2026-11-18T01:05:30+08:00"}`, "start_time"},
		{"缺少结束时间", `{"job_name":"db","status":"success","start_time":"2026-11-18T01:00:00+08:00"}`, "end_time"},
		{"结束时间早于开始时间", valid("db", "success", "2026-11-18T02:00:00+08:00", "2026-11-18T01:00:00+08:00", ""), "before start_time"},
		{"字段值为 null", `{"job_name":"db","status":"success","start_time":null,"end_time":null}`, "start_time"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseTaskResult([]byte(tc.data))
			if err == nil {
				t.Fatalf("expected error, got result %+v", r)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestTaskResult_Duration 验证耗时计算口径：结束时间减开始时间。
func TestTaskResult_Duration(t *testing.T) {
	data := []byte(`{
		"job_name": "file_sync",
		"status": "success",
		"start_time": "2026-11-18T01:00:00+08:00",
		"end_time": "2026-11-18T01:05:30+08:00",
		"exit_code": 0
	}`)
	r, err := ParseTaskResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 5*time.Minute + 30*time.Second
	if got := r.Duration(); got != want {
		t.Errorf("Duration = %v, want %v", got, want)
	}
}

// TestTaskResult_DurationZero 时间计算边界：开始和结束相同，耗时为零。
// 指标上表现为 ops_job_last_duration_seconds = 0，是合法值而非错误。
func TestTaskResult_DurationZero(t *testing.T) {
	data := []byte(`{
		"job_name": "fast_check",
		"status": "success",
		"start_time": "2026-11-20T01:00:00+08:00",
		"end_time": "2026-11-20T01:00:00+08:00",
		"exit_code": 0
	}`)
	r, err := ParseTaskResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := r.Duration(); got != 0 {
		t.Errorf("Duration = %v, want 0", got)
	}
}

// TestParseTaskResult_RobustExtras 健壮性：任务方在 JSON 里多写字段、
// 退出码为负、message 含中文/转义字符，都不能影响解析。
// 未知字段必须被忽略（向后兼容：未来任务方加字段不需要升级 exporter）。
func TestParseTaskResult_RobustExtras(t *testing.T) {
	data := []byte(`{
		"job_name": "database_backup",
		"status": "success",
		"start_time": "2026-11-20T01:00:00+08:00",
		"end_time": "2026-11-20T01:05:30+08:00",
		"exit_code": -2,
		"message": "备份完成，共 128 张表 \"utf8\"",
		"host": "db-01",
		"future_field": {"nested": [1, 2, 3]}
	}`)
	r, err := ParseTaskResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ExitCode != -2 {
		t.Errorf("ExitCode = %d, want -2", r.ExitCode)
	}
	if !strings.Contains(r.Message, "备份完成") {
		t.Errorf("Message = %q, want unicode content preserved", r.Message)
	}
	// 未知字段不进入结构体（结构体字段数固定），这里只需确认解析成功
}
