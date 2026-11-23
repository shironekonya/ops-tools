package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNormalizeStatus 四种合法状态映射，未知/空值返回 false。
func TestNormalizeStatus(t *testing.T) {
	cases := []struct {
		jenkins string
		want    string
		ok      bool
	}{
		{JenkinsSuccess, StatusSuccess, true},
		{JenkinsFailure, StatusFailed, true},
		{JenkinsUnstable, StatusUnstable, true},
		{JenkinsAborted, StatusAborted, true},
		{"RUNNING", "", false},
		{"", "", false},
		{"success", "", false}, // 大小写敏感：Jenkins 返回大写
	}
	for _, tc := range cases {
		t.Run(tc.jenkins, func(t *testing.T) {
			got, ok := NormalizeStatus(tc.jenkins)
			if ok != tc.ok || got != tc.want {
				t.Errorf("NormalizeStatus(%q) = (%q,%v), want (%q,%v)", tc.jenkins, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestToBuildResult 验证时间换算（开始+耗时=结束，ms→s）与状态映射。
func TestToBuildResult(t *testing.T) {
	info := BuildInfo{
		JobName:     "deploy",
		Number:      42,
		Result:      JenkinsFailure,
		TimestampMs: 1700000000000, // 开始：2023-11-14T22:13:20Z
		DurationMs:  123500,        // 耗时 123.5s
	}
	got, ok := ToBuildResult(info, "deploy-stage")
	if !ok {
		t.Fatal("expected ok")
	}
	if got.JobName != "deploy" || got.BuildNumber != 42 {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.Status != StatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.Duration != 123.5 {
		t.Errorf("Duration = %v, want 123.5", got.Duration)
	}
	// 结束时间 = (1700000000000+123500)/1000 = 1700000123
	if got.Timestamp != 1700000123 {
		t.Errorf("Timestamp = %d, want 1700000123", got.Timestamp)
	}
	if got.FailedStage != "deploy-stage" {
		t.Errorf("FailedStage = %q", got.FailedStage)
	}
}

// TestToBuildResult_UnknownStatus 未知状态（构建中）返回 ok=false。
func TestToBuildResult_UnknownStatus(t *testing.T) {
	_, ok := ToBuildResult(BuildInfo{JobName: "x", Result: "RUNNING"}, "")
	if ok {
		t.Fatal("unknown status should not convert")
	}
}

// TestBuildResult_JSONContract 验证 JSON tag 与 Exporter 契约一致：
// 字段名严格 job_name/build_number/status/duration/timestamp/failed_stage，
// 且 Bytes 能被 Exporter 同构结构解析（这里用字符串断言 key 与值）。
func TestBuildResult_JSONContract(t *testing.T) {
	b := BuildResult{
		JobName: "p", BuildNumber: 7, Status: StatusSuccess,
		Duration: 1.5, Timestamp: 1700000000,
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(data)
	for _, key := range []string{
		`"job_name":`, `"build_number":`, `"status":`, `"duration":`, `"timestamp":`,
	} {
		if !strings.Contains(text, key) {
			t.Errorf("JSON missing contract key %q: %s", key, text)
		}
	}
	// failed_stage 为空时 omitempty 生效，不出现该 key。
	if strings.Contains(text, "failed_stage") {
		t.Errorf("empty failed_stage should be omitted: %s", text)
	}
}
