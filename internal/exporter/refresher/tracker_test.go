package refresher

// 本文件验证 TODO 9 的日志降噪行为。
// 通过把 slog 默认 logger 换成写入内存 buffer 的 handler，
// 断言各级别日志的输出时机。

import (
	"bytes"
	"errors"
	"log/slog"
	"ops-exporter/internal/exporter/reader"
	"os"
	"strings"
	"testing"
)

// captureLogs 把 slog 默认输出捕获到 buffer（Debug 级别全开），
// 返回 buffer 和恢复函数。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	})
	return &buf
}

func fe(path string) reader.FileError {
	return reader.FileError{Path: path, Err: errors.New("parse: boom")}
}

// TestRefreshTracker_FileFailure_Dedup 验收"不输出无意义的重复日志"：
// 同一个文件连续失败，只有第一次 Warn，之后降为 Debug。
func TestRefreshTracker_FileFailure_Dedup(t *testing.T) {
	buf := captureLogs(t)
	tr := newRefreshTracker()

	tr.reportFiles([]reader.FileError{fe("data/backup.json")})
	tr.reportFiles([]reader.FileError{fe("data/backup.json")})
	tr.reportFiles([]reader.FileError{fe("data/backup.json")})

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("got %d WARN lines, want exactly 1:\n%s", n, out)
	}
	if n := strings.Count(out, "level=DEBUG"); n != 2 {
		t.Errorf("got %d DEBUG lines, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "consecutive_failures=3") {
		t.Errorf("repeated failure should carry count:\n%s", out)
	}
	// 首次 Warn 必须带文件路径，能定位到具体任务文件
	if !strings.Contains(out, `path=data/backup.json`) {
		t.Errorf("WARN should carry file path:\n%s", out)
	}
}

// TestRefreshTracker_FileRecovered_Silent 验证文件不再失败时静默移出跟踪：
// 无法区分"修好了"还是"删了"，所以不发日志（不猜）。
func TestRefreshTracker_FileRecovered_Silent(t *testing.T) {
	buf := captureLogs(t)
	tr := newRefreshTracker()

	tr.reportFiles([]reader.FileError{fe("data/backup.json")})
	buf.Reset() // 丢弃首次 Warn，只看恢复阶段
	tr.reportFiles(nil)

	if out := buf.String(); out != "" {
		t.Errorf("recovery should be silent, got:\n%s", out)
	}

	// 移出跟踪后再次失败应重新从 Warn 开始（新一轮故障）
	buf.Reset()
	tr.reportFiles([]reader.FileError{fe("data/backup.json")})
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Errorf("new failure round should start with WARN again, got:\n%s", buf.String())
	}
}

// TestRefreshTracker_DirLifecycle 验证目录级错误的完整生命周期：
// 首次 Error → 重复 Debug → 恢复 Info → 再失败重新 Error。
func TestRefreshTracker_DirLifecycle(t *testing.T) {
	buf := captureLogs(t)
	tr := newRefreshTracker()
	dirErr := errors.New("read task result dir: permission denied")

	tr.reportDir(dirErr) // 第 1 次：Error
	tr.reportDir(dirErr) // 第 2 次：Debug
	tr.reportDir(dirErr) // 第 3 次：Debug
	tr.reportDir(nil)    // 恢复：Info
	tr.reportDir(dirErr) // 新一轮第 1 次：Error

	out := buf.String()
	if n := strings.Count(out, "level=ERROR"); n != 2 {
		t.Errorf("got %d ERROR lines, want 2 (first of each round):\n%s", n, out)
	}
	if !strings.Contains(out, "level=INFO msg=\"task result directory readable again\"") {
		t.Errorf("recovery should log Info:\n%s", out)
	}
	if !strings.Contains(out, "consecutive_failures=2") {
		t.Errorf("repeated dir failure should carry count:\n%s", out)
	}
}

// TestRefreshTracker_MultipleFilesIndependent 验证不同文件的失败次数互不干扰：
// A 持续失败时 B 修复又复发，A 不受影响，B 重新从 Warn 开始。
func TestRefreshTracker_MultipleFilesIndependent(t *testing.T) {
	buf := captureLogs(t)
	tr := newRefreshTracker()

	tr.reportFiles([]reader.FileError{fe("a.json")})
	tr.reportFiles([]reader.FileError{fe("a.json"), fe("b.json")})
	tr.reportFiles([]reader.FileError{fe("a.json")})

	out := buf.String()

	// 逐行统计各文件、各级别的条数
	warnA, debugA, warnB := 0, 0, 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "invalid task result file") {
			continue
		}
		switch {
		case strings.Contains(line, "path=a.json") && strings.Contains(line, "level=WARN"):
			warnA++
		case strings.Contains(line, "path=a.json") && strings.Contains(line, "level=DEBUG"):
			debugA++
		case strings.Contains(line, "path=b.json") && strings.Contains(line, "level=WARN"):
			warnB++
		}
	}

	// a.json：1 Warn + 2 Debug
	if warnA != 1 || debugA != 2 {
		t.Errorf("a.json: got %d WARN + %d DEBUG, want 1 WARN + 2 DEBUG:\n%s", warnA, debugA, out)
	}
	// b.json：只失败过一次，应该只有 Warn
	if warnB != 1 {
		t.Errorf("b.json: got %d WARN, want 1:\n%s", warnB, out)
	}
}
