// Package parser 从 Jenkins console log 中提取结构化信息：失败阶段、异常摘要、错误行。
//
// 与 jenkins client 严格分离：client 只取回原始文本，解析逻辑全在这里。
// 设计约束（来自任务文档）：
//   - 不一开始就对完整 Console Log 做复杂正则；用关键词 + 有限状态即可；
//   - 不把完整日志放进指标标签——因此摘要与阶段名都有长度上限，
//     ErrorLines 也有条数上限，调用方只取需要的短文本。
package parser

import (
	"regexp"
	"strings"
)

// 长度上限：防止异常日志把标签/存储撑爆。
const (
	maxSummaryBytes     = 300 // 异常摘要最大字节数
	maxFailedStageBytes = 80  // 失败阶段名最大字节数
	maxErrorLines       = 20  // 最多保留的错误行数
)

// 常见错误关键词（大小写不敏感匹配）。
var errorKeywords = []string{
	"error:", "failed", "failure", "exception", "fatal", "BUILD FAILURE",
}

// 流水线阶段行的常见格式：
//
//	[Pipeline] Stage: Deploy
//	Stage: Deploy
//	+ stage("Deploy")
var stagePattern = regexp.MustCompile(`(?i)(?:\[Pipeline\]\s*)?Stage:\s*([^\r\n]+)`)

// Result 是日志解析结果。
type Result struct {
	FailedStage string   // 识别到的最后一个失败阶段（已截断清洗）
	Summary     string   // 异常摘要（有限长度的若干错误行拼接）
	ErrorLines  []string // 命中的错误行（有上限）
}

// Parse 解析 console log。
//
// 策略：
//  1. 逐行扫描，命中错误关键词的行记入 ErrorLines；
//  2. 命中 Stage: 的行更新 FailedStage（流水线走到最后一个阶段通常即失败点）；
//  3. Summary 取前若干条 ErrorLines 拼接并截断到 maxSummaryBytes。
func Parse(log string) Result {
	r := Result{ErrorLines: make([]string, 0)}
	var stage string

	for _, raw := range strings.Split(log, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		if m := stagePattern.FindStringSubmatch(line); len(m) == 2 {
			stage = truncate(strings.TrimSpace(m[1]), maxFailedStageBytes)
		}

		if isErrorLine(line) && len(r.ErrorLines) < maxErrorLines {
			r.ErrorLines = append(r.ErrorLines, truncate(line, maxSummaryBytes))
		}
	}

	r.FailedStage = stage
	r.Summary = buildSummary(r.ErrorLines)
	return r
}

// isErrorLine 判断一行是否命中常见错误关键词（大小写不敏感）。
func isErrorLine(line string) bool {
	lower := strings.ToLower(line)
	for _, kw := range errorKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// buildSummary 把错误行拼接成有限长度的摘要。
func buildSummary(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteString(" | ")
		}
		remaining := maxSummaryBytes - b.Len()
		if remaining <= 0 {
			break
		}
		if len(line) > remaining {
			b.WriteString(line[:remaining])
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// truncate 按字节截断，避免超长文本进入下游。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
