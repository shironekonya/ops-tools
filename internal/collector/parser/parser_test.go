package parser

import (
	"strings"
	"testing"
)

// TestParse_FailedStage 识别 Stage: 行，取最后一个（流水线失败点）。
func TestParse_FailedStage(t *testing.T) {
	log := `[Pipeline] Stage: Checkout
[Pipeline] Stage: Build
[Pipeline] Stage: Deploy
ERROR: deploy failed`
	r := Parse(log)
	if r.FailedStage != "Deploy" {
		t.Errorf("FailedStage = %q, want Deploy", r.FailedStage)
	}
}

// TestParse_ErrorLines 命中常见错误关键词的行被记录。
func TestParse_ErrorLines(t *testing.T) {
	log := strings.Join([]string{
		"Started by user admin",
		"[Pipeline] Stage: Build",
		"ERROR: compilation failed",
		"java.lang.NullPointerException at Foo.java:12",
		"FATAL: cannot continue",
		"Build step 'Execute shell' marked build as failure",
	}, "\n")
	r := Parse(log)
	if len(r.ErrorLines) < 3 {
		t.Fatalf("ErrorLines = %v, want >= 3", r.ErrorLines)
	}
	if !strings.Contains(strings.Join(r.ErrorLines, " "), "NullPointerException") {
		t.Errorf("ErrorLines missing exception: %v", r.ErrorLines)
	}
}

// TestParse_CleanLog 干净日志无错误、无阶段时不产生内容。
func TestParse_CleanLog(t *testing.T) {
	r := Parse("Started\nAll tests passed\nFinished: SUCCESS")
	if len(r.ErrorLines) != 0 || r.FailedStage != "" || r.Summary != "" {
		t.Errorf("clean log should produce empty result: %+v", r)
	}
}

// TestParse_SummaryLimited 摘要与阶段名有长度上限，防止日志进标签撑爆基数。
func TestParse_SummaryLimited(t *testing.T) {
	longErr := "ERROR: " + strings.Repeat("x", 1000)
	r := Parse(longErr + "\nStage: " + strings.Repeat("s", 500))
	if len(r.Summary) > maxSummaryBytes {
		t.Errorf("Summary len = %d, want <= %d", len(r.Summary), maxSummaryBytes)
	}
	if len(r.FailedStage) > maxFailedStageBytes {
		t.Errorf("FailedStage len = %d, want <= %d", len(r.FailedStage), maxFailedStageBytes)
	}
}

// TestParse_ErrorLineLimit 错误行数有上限。
func TestParse_ErrorLineLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("ERROR: failure\n")
	}
	r := Parse(b.String())
	if len(r.ErrorLines) != maxErrorLines {
		t.Errorf("ErrorLines = %d, want %d", len(r.ErrorLines), maxErrorLines)
	}
}

// TestParse_MixedCase 关键词大小写不敏感。
func TestParse_MixedCase(t *testing.T) {
	r := parseHelper("error: lower case\n[Pipeline] stage: Mixed")
	if len(r.ErrorLines) == 0 {
		t.Error("lowercase 'error:' should match")
	}
	if r.FailedStage != "Mixed" {
		t.Errorf("FailedStage = %q, want Mixed (case-insensitive stage match)", r.FailedStage)
	}
}

func parseHelper(log string) Result { return Parse(log) }
