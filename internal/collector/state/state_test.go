package state

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadSaveRoundTrip 保存后加载：断点内容完整还原。
func TestLoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json") // 验证自动建目录
	want := State{Seen: map[string]int64{"job-a": 42, "job-b": 7}}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Seen) != 2 || got.Seen["job-a"] != 42 || got.Seen["job-b"] != 7 {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	// 原子写：不应残留临时文件。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should be renamed away")
	}
}

// TestLoadMissingAndEmpty 文件不存在/空文件都返回空状态（首次启动正常路径）。
func TestLoadMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := Load(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(st.Seen) != 0 {
		t.Errorf("missing file should give empty state, got %+v", st)
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = Load(empty)
	if err != nil {
		t.Fatalf("empty file should not error: %v", err)
	}
	if len(st.Seen) != 0 {
		t.Errorf("empty file should give empty state, got %+v", st)
	}
}

// TestLoadCorrupted 损坏文件返回错误（调用方负责降级为空状态）。
func TestLoadCorrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"seen":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("corrupted state should return error")
	}
}

// TestSaveEmptyPath 空路径是禁用持久化的约定，Save/Load 都不报错。
func TestSaveEmptyPath(t *testing.T) {
	if err := Save("", State{Seen: map[string]int64{"a": 1}}); err != nil {
		t.Fatalf("Save with empty path: %v", err)
	}
	if _, err := Load(""); err != nil {
		t.Fatalf("Load with empty path: %v", err)
	}
}
