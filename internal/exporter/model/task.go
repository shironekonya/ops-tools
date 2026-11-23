package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// 任务状态的两个合法取值。
// 第一版只有成功和失败两种，未来扩展时在此补充。
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// TaskResult 对应外部任务写入的结果 JSON 文件内容，
// 描述某个任务最近一次的执行结果。
//
// 字段与 TODO 文档中约定的格式一一对应：
//
//	{
//	  "job_name":  "database_backup",
//	  "status":    "success",
//	  "start_time": "2026-11-18T01:00:00+08:00",
//	  "end_time":  "2026-11-18T01:05:30+08:00",
//	  "exit_code": 0,
//	  "message":   "backup completed"
//	}
type TaskResult struct {
	// JobName 任务名称，作为后续指标中的 job_name 标签，必填。
	JobName string `json:"job_name"`

	// Status 任务状态，只允许 "success" 或 "failed"，必填。
	Status string `json:"status"`

	// StartTime 任务开始时间，RFC3339 格式（如 2026-11-18T01:00:00+08:00），必填。
	// time.Time 让 encoding/json 直接按 RFC3339 解析，
	// 格式错误会在 Unmarshal 阶段报错，不需要手动解析字符串。
	StartTime time.Time `json:"start_time"`

	// EndTime 任务结束时间，RFC3339 格式，必填。
	EndTime time.Time `json:"end_time"`

	// ExitCode 任务退出码，0 通常表示成功，但以 Status 字段为准。
	ExitCode int `json:"exit_code"`

	// Message 执行结果说明，选填，仅用于排查问题，
	// 不允许进入 Prometheus 指标的 Label（内容不可控）。
	Message string `json:"message"`
}

// ParseTaskResult 把一段 JSON 字节解析并校验为 TaskResult。
//
// 校验失败时返回带原因的 error，调用方据此决定跳过还是记录日志；
// 本函数保证不 panic，任何异常数据都以 error 形式返回。
func ParseTaskResult(data []byte) (TaskResult, error) {
	var r TaskResult
	if err := json.Unmarshal(data, &r); err != nil {
		// JSON 语法错误或时间格式错误都在这一步暴露。
		return TaskResult{}, fmt.Errorf("invalid task result JSON: %w", err)
	}
	if err := r.validate(); err != nil {
		return TaskResult{}, err
	}
	return r, nil
}

// validate 检查解析后的字段是否符合业务约定。
func (r TaskResult) validate() error {
	if r.JobName == "" {
		return fmt.Errorf("missing required field: job_name")
	}
	if r.Status != StatusSuccess && r.Status != StatusFailed {
		return fmt.Errorf("invalid status %q: must be %q or %q",
			r.Status, StatusSuccess, StatusFailed)
	}
	if r.StartTime.IsZero() {
		return fmt.Errorf("missing required field: start_time")
	}
	if r.EndTime.IsZero() {
		return fmt.Errorf("missing required field: end_time")
	}
	if r.EndTime.Before(r.StartTime) {
		return fmt.Errorf("end_time %s is before start_time %s",
			r.EndTime.Format(time.RFC3339), r.StartTime.Format(time.RFC3339))
	}
	return nil
}

// Duration 返回任务执行耗时（结束时间减开始时间）。
// 这是 TODO 5 中 ops_job_last_duration_seconds 指标的数据来源，
// 放在这里保证耗时口径只有一处。
func (r TaskResult) Duration() time.Duration {
	return r.EndTime.Sub(r.StartTime)
}
