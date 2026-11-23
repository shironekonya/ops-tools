package model

import (
	"strings"
	"testing"
	"time"
)

// 一个确定的过去时间戳（2023-11-14T22:13:20Z），远早于任何运行时刻。
// ParseBuildResult 内部用 time.Now() 做"未来偏移"校验，用过去时间可保证合法样例永不被误伤。
const sampleTimestamp int64 = 1700000000

// 合法 Build 发射器：各个非法用例只改一个字段，避免互相干扰判断。
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

// TestParseBuildResult_Valid 验证完整合法 JSON 的每个字段都被正确解析。
func TestParseBuildResult_Valid(t *testing.T) {
	b, err := ParseBuildResult([]byte(validBuildJSON()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.JobName != "my-pipeline" {
		t.Errorf("JobName = %q, want %q", b.JobName, "my-pipeline")
	}
	if b.BuildNumber != 42 {
		t.Errorf("BuildNumber = %d, want 42", b.BuildNumber)
	}
	if b.Status != BuildStatusFailed {
		t.Errorf("Status = %q, want %q", b.Status, BuildStatusFailed)
	}
	if b.Duration != 123.5 {
		t.Errorf("Duration = %v, want 123.5", b.Duration)
	}
	if b.Timestamp != sampleTimestamp {
		t.Errorf("Timestamp = %d, want %d", b.Timestamp, sampleTimestamp)
	}
	if b.FailedStage != "deploy" {
		t.Errorf("FailedStage = %q, want %q", b.FailedStage, "deploy")
	}
}

// TestParseBuildResult_AllStatuses 四种合法状态都必须通过校验。
func TestParseBuildResult_AllStatuses(t *testing.T) {
	for _, status := range []string{
		BuildStatusSuccess, BuildStatusFailed, BuildStatusUnstable, BuildStatusAborted,
	} {
		t.Run(status, func(t *testing.T) {
			data := strings.Replace(validBuildJSON(), `"status": "failed"`, `"status": "`+status+`"`, 1)
			b, err := ParseBuildResult([]byte(data))
			if err != nil {
				t.Fatalf("status %q should be valid, got error: %v", status, err)
			}
			if b.Status != status {
				t.Errorf("Status = %q, want %q", b.Status, status)
			}
		})
	}
}

// TestParseBuildResult_Invalid 集中覆盖各种非法输入：
// 每种情况都必须返回 error，且不允许 panic。
func TestParseBuildResult_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantErr string // 期望错误信息包含的片段
	}{
		{"JSON 语法错误", `{"job_name": "x", `, "invalid build JSON"},
		{"完全不是 JSON", `not a json`, "invalid build JSON"},
		{"空字节", ``, "invalid build JSON"},
		{"job_name 为空", strings.Replace(validBuildJSON(), `"my-pipeline"`, `""`, 1), "job_name"},
		{"缺少 job_name", `{"build_number":42,"status":"success","duration":1,"timestamp":1700000000}`, "job_name"},
		{"build_number 为零", strings.Replace(validBuildJSON(), `"build_number": 42`, `"build_number": 0`, 1), "build_number"},
		{"build_number 为负", strings.Replace(validBuildJSON(), `"build_number": 42`, `"build_number": -1`, 1), "build_number"},
		{"缺少 build_number", `{"job_name":"x","status":"success","duration":1,"timestamp":1700000000}`, "build_number"},
		{"status 为空", strings.Replace(validBuildJSON(), `"status": "failed"`, `"status": ""`, 1), "status"},
		{"status 非法", strings.Replace(validBuildJSON(), `"status": "failed"`, `"status": "running"`, 1), "invalid status"},
		{"缺少 status", `{"job_name":"x","build_number":1,"duration":1,"timestamp":1700000000}`, "status"},
		{"duration 为负", strings.Replace(validBuildJSON(), `"duration": 123.5`, `"duration": -0.1`, 1), "duration"},
		{"timestamp 为零", strings.Replace(validBuildJSON(), `"timestamp": 1700000000`, `"timestamp": 0`, 1), "timestamp"},
		{"timestamp 为负", strings.Replace(validBuildJSON(), `"timestamp": 1700000000`, `"timestamp": -5`, 1), "timestamp"},
		{"缺少 timestamp", `{"job_name":"x","build_number":1,"status":"success","duration":1}`, "timestamp"},
		{"timestamp 太超前", strings.Replace(validBuildJSON(), `"timestamp": 1700000000`, `"timestamp": 4102444800`, 1), "future"},
		{"build_number 类型错误", strings.Replace(validBuildJSON(), `"build_number": 42`, `"build_number": "42"`, 1), "invalid build JSON"},
		{"duration 类型错误", strings.Replace(validBuildJSON(), `"duration": 123.5`, `"duration": "123.5"`, 1), "invalid build JSON"},
		{"status 类型错误", strings.Replace(validBuildJSON(), `"status": "failed"`, `"status": 1`, 1), "invalid build JSON"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := ParseBuildResult([]byte(tc.data))
			if err == nil {
				t.Fatalf("expected error, got result %+v", b)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestBuildResult_ValidateFutureSkew 用注入的 now 精确测试时钟偏差边界，
// 避免依赖真实时间造成测试不稳定。
func TestBuildResult_ValidateFutureSkew(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		timestamp int64
		wantErr   bool
	}{
		{"过去时间", now.Add(-1 * time.Hour).Unix(), false},
		{"当前时间", now.Unix(), false},
		{"未来 1 分钟（偏差内）", now.Add(1 * time.Minute).Unix(), false},
		{"恰好 +5 分钟（边界）", now.Add(maxFutureSkew).Unix(), false},
		{"+5 分钟 1 秒（越界）", now.Add(maxFutureSkew + time.Second).Unix(), true},
		{"未来 1 小时", now.Add(1 * time.Hour).Unix(), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := BuildResult{
				JobName:     "pipeline",
				BuildNumber: 1,
				Status:      BuildStatusSuccess,
				Duration:    10,
				Timestamp:   tc.timestamp,
			}
			err := b.validate(now)
			if tc.wantErr && err == nil {
				t.Fatalf("timestamp %d should be rejected", tc.timestamp)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("timestamp %d should be accepted, got: %v", tc.timestamp, err)
			}
		})
	}
}

// TestBuildResult_DurationZero duration 为 0 是合法构建（瞬时完成），不是错误。
func TestBuildResult_DurationZero(t *testing.T) {
	data := strings.Replace(validBuildJSON(), `"duration": 123.5`, `"duration": 0`, 1)
	b, err := ParseBuildResult([]byte(data))
	if err != nil {
		t.Fatalf("zero duration should be valid, got: %v", err)
	}
	if b.Duration != 0 {
		t.Errorf("Duration = %v, want 0", b.Duration)
	}
}

// TestBuildResult_EndTime 验证 timestamp→time.Time 的换算口径唯一且正确。
func TestBuildResult_EndTime(t *testing.T) {
	b := BuildResult{Timestamp: sampleTimestamp}
	want := time.Unix(sampleTimestamp, 0)
	if got := b.EndTime(); !got.Equal(want) {
		t.Errorf("EndTime = %s, want %s", got, want)
	}
}

// TestParseBuildResult_RobustExtras 健壮性：未知字段、Unicode 任务名、
// 选填 failed_stage 缺省都不能影响解析（向后兼容 Collector 侧加字段）。
func TestParseBuildResult_RobustExtras(t *testing.T) {
	data := []byte(`{
		"job_name": "流水线-部署",
		"build_number": 7,
		"status": "unstable",
		"duration": 0.5,
		"timestamp": 1700000000,
		"future_field": {"nested": [1, 2, 3]}
	}`)
	b, err := ParseBuildResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.JobName != "流水线-部署" {
		t.Errorf("JobName = %q, want unicode name preserved", b.JobName)
	}
	if b.FailedStage != "" {
		t.Errorf("FailedStage = %q, want empty when omitted", b.FailedStage)
	}
	if b.Duration != 0.5 {
		t.Errorf("Duration = %v, want 0.5", b.Duration)
	}
}
