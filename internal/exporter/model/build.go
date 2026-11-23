package model

// 本文件实现动态化改造 Task 2 第一部分：Jenkins Build 数据模型与校验。
//
// 设计对齐 task.go 的模式（模型 + Parse 函数 + validate 方法），
// 但刻意不复用 TaskResult：两者字段集和语义不同
// （build 有 build_number/duration/failed_stage，没有 start/end 时间对），
// 强行合并会造出"什么都装"的上帝模型（任务文档明确禁止通用动态框架）。
//
// 时间约定：timestamp 是构建结束时刻的 Unix 秒。用整数而不是 RFC3339 字符串，
// 是因为 Collector 从 Jenkins API 拿到的本来就是毫秒时间戳，换算成秒直接传，
// 避免 Collector 侧再做一次时区字符串格式化。

import (
	"encoding/json"
	"fmt"
	"time"
)

// Jenkins 构建的合法状态取值。
// 对齐 Jenkins 的 Build 结果语义，但用小写与 ops_job_* 生态保持风格一致：
//   - success：构建成功
//   - failed：构建失败（failure_total 只统计这个状态）
//   - unstable：构建完成但有测试失败等非致命问题
//   - aborted：被人工取消或超时终止
const (
	BuildStatusSuccess  = "success"
	BuildStatusFailed   = "failed"
	BuildStatusUnstable = "unstable"
	BuildStatusAborted  = "aborted"
)

// maxFutureSkew 允许的 timestamp 未来偏移上限。
// Collector 与 Exporter 时钟可能有少量偏差，完全拒绝未来时间会误伤合法数据；
// 但超过 5 分钟的"未来"几乎必然是 Collector 侧的时间换算 bug，
// 这种数据进指标会让 time() - jenkins_build_last_timestamp 出现负值，必须拒绝。
const maxFutureSkew = 5 * time.Minute

// BuildResult 对应 POST /api/v1/ingest/build 的请求体，
// 描述一次 Jenkins 构建的最终结果。
//
//	{
//	  "job_name":     "my-pipeline",
//	  "build_number": 42,
//	  "status":       "failed",
//	  "duration":     123.5,
//	  "timestamp":    1795160000,
//	  "failed_stage": "deploy"
//	}
type BuildResult struct {
	// JobName Jenkins 任务名（流水线名），作为 jenkins_* 指标的 job_name 标签，必填。
	JobName string `json:"job_name"`

	// BuildNumber 构建编号，Jenkins 中同一 Job 单调递增，必填且必须为正。
	// 它是"新旧判定"的依据（BuildStore 用它去重），但**不作为指标标签**（基数随构建次数增长）。
	BuildNumber int64 `json:"build_number"`

	// Status 构建状态，只允许上表四个取值之一，必填。
	Status string `json:"status"`

	// Duration 构建耗时（秒，可为小数），必填且非负。
	Duration float64 `json:"duration"`

	// Timestamp 构建结束时刻的 Unix 秒，必填。作为 last_timestamp 指标值。
	Timestamp int64 `json:"timestamp"`

	// FailedStage 失败阶段名（如 "deploy"），选填。
	// 仅用于日志排查，**不允许进入指标标签**：阶段名来自流水线定义，
	// 虽然理论上基数有限，但一个流水线几十个阶段 × 所有流水线 = 不可控，
	// 且失败原因分析属于日志范畴，不属于指标维度。
	FailedStage string `json:"failed_stage"`
}

// ParseBuildResult 把一段 JSON 字节解析并校验为 BuildResult。
//
// 校验失败返回带原因的 error，HTTP handler 据此返回 400；
// 本函数保证不 panic，任何异常输入都以 error 形式返回。
func ParseBuildResult(data []byte) (BuildResult, error) {
	var b BuildResult
	if err := json.Unmarshal(data, &b); err != nil {
		return BuildResult{}, fmt.Errorf("invalid build JSON: %w", err)
	}
	if err := b.validate(time.Now()); err != nil {
		return BuildResult{}, err
	}
	return b, nil
}

// validate 检查解析后的字段是否符合业务约定。
// now 由调用方注入而不是内部取 time.Now()，是为了测试可以构造边界时间。
func (b BuildResult) validate(now time.Time) error {
	if b.JobName == "" {
		return fmt.Errorf("missing required field: job_name")
	}
	if b.BuildNumber <= 0 {
		return fmt.Errorf("invalid build_number %d: must be positive", b.BuildNumber)
	}
	switch b.Status {
	case BuildStatusSuccess, BuildStatusFailed, BuildStatusUnstable, BuildStatusAborted:
		// 合法状态
	case "":
		return fmt.Errorf("missing required field: status")
	default:
		return fmt.Errorf("invalid status %q: must be one of %q, %q, %q, %q",
			b.Status, BuildStatusSuccess, BuildStatusFailed, BuildStatusUnstable, BuildStatusAborted)
	}
	if b.Duration < 0 {
		return fmt.Errorf("invalid duration %v: must be non-negative", b.Duration)
	}
	if b.Timestamp <= 0 {
		return fmt.Errorf("missing required field: timestamp")
	}
	// timestamp 不得超过 now + maxFutureSkew（时钟偏差保护，见 maxFutureSkew 注释）。
	futureLimit := now.Add(maxFutureSkew)
	if b.Timestamp > futureLimit.Unix() {
		return fmt.Errorf("invalid timestamp %d: more than %s in the future",
			b.Timestamp, maxFutureSkew)
	}
	return nil
}

// EndTime 返回构建结束时刻（timestamp 的 time.Time 形式）。
// 指标转换（buildupdater）统一从这里取时间，保证口径只有一处——
// 与 task.go 的 Duration() 同样的设计理由。
func (b BuildResult) EndTime() time.Time {
	return time.Unix(b.Timestamp, 0)
}
