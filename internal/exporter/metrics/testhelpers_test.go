package metrics

import "ops-exporter/internal/exporter/model"

// 测试专用别名与常量：拆包前这些符号与 JobMetrics 在同一个 package main 里，
// 拆包后 BuildResult 相关类型归属 model 包，用别名让原有测试代码零改动。
type BuildResult = model.BuildResult

const (
	BuildStatusSuccess  = model.BuildStatusSuccess
	BuildStatusFailed   = model.BuildStatusFailed
	BuildStatusUnstable = model.BuildStatusUnstable
	BuildStatusAborted  = model.BuildStatusAborted
	sampleTimestamp     = int64(1700000000)
)
