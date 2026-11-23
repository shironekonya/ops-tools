// Package model 定义 Collector 的内部数据模型，以及与 Exporter POST 契约对齐的 JSON 结构。
//
// 关键约定：BuildResult 的 JSON tag 必须与 Exporter 的 build.go 完全一致
// （job_name/build_number/status/duration/timestamp/failed_stage），
// 因为两个 module 独立编译、没有共享代码，靠这份"结构化契约"对接。
package model

// Jenkins Build 的合法结果取值（Jenkins REST API 返回的大写形式）。
const (
	JenkinsSuccess  = "SUCCESS"
	JenkinsFailure  = "FAILURE"
	JenkinsUnstable = "UNSTABLE"
	JenkinsAborted  = "ABORTED"
)

// Exporter 接受的状态取值（小写，与 Exporter build.go 一致）。
const (
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusUnstable = "unstable"
	StatusAborted  = "aborted"
)

// BuildInfo 是从 Jenkins REST API 结构化构建信息中直接读取的字段。
// 优先用这些结构化数据，而不是解析完整 Console Log（任务文档要求）。
type BuildInfo struct {
	JobName     string
	Number      int64  // build_number
	Result      string // Jenkins 原始结果：SUCCESS/FAILURE/UNSTABLE/ABORTED
	TimestampMs int64  // Jenkins 开始时间的毫秒时间戳
	DurationMs  int64  // Jenkins 执行耗时的毫秒数
}

// NormalizeStatus 把 Jenkins 结果映射为 Exporter 接受的状态。
// 未知/空值（如正在构建中的 null result）返回 ok=false，调用方应跳过。
func NormalizeStatus(jenkinsResult string) (string, bool) {
	switch jenkinsResult {
	case JenkinsSuccess:
		return StatusSuccess, true
	case JenkinsFailure:
		return StatusFailed, true
	case JenkinsUnstable:
		return StatusUnstable, true
	case JenkinsAborted:
		return StatusAborted, true
	default:
		return "", false
	}
}

// BuildResult 对应 Exporter 的 POST /api/v1/ingest/build 请求体。
// JSON tag 必须与 Exporter build.go 的 BuildResult 严格一致。
type BuildResult struct {
	JobName     string  `json:"job_name"`
	BuildNumber int64   `json:"build_number"`
	Status      string  `json:"status"`
	Duration    float64 `json:"duration"`
	Timestamp   int64   `json:"timestamp"`
	FailedStage string  `json:"failed_stage,omitempty"`
}

// ToBuildResult 把 Jenkins 结构化构建信息 + 日志解析结果转换为 Exporter 契约数据。
//
// 时间换算：Jenkins 给的 timestamp 是**开始时间**（毫秒），duration 是耗时（毫秒）。
// Exporter 的 timestamp 约定为**构建结束时刻的 Unix 秒**（见 Exporter build.go），
// 因此结束时间 = 开始时间 + 耗时，再除以 1000 转为秒。
//
// 状态无法识别时返回 ok=false，调用方跳过该 Build（如仍在构建中）。
func ToBuildResult(info BuildInfo, failedStage string) (BuildResult, bool) {
	status, ok := NormalizeStatus(info.Result)
	if !ok {
		return BuildResult{}, false
	}
	endMs := info.TimestampMs + info.DurationMs
	return BuildResult{
		JobName:     info.JobName,
		BuildNumber: info.Number,
		Status:      status,
		Duration:    float64(info.DurationMs) / 1000.0,
		Timestamp:   endMs / 1000,
		FailedStage: failedStage,
	}, true
}
