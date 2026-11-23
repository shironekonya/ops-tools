// Package state 把 Collector 的"已处理 Build"断点持久化到磁盘。
//
// 为什么需要：seen map 目前只在内存里，进程重启后清空，导致已经成功发送过的
// Build 被重新采集、重新 POST（Exporter 虽幂等返回 ignored，但会产生无意义流量
// 和日志噪音）。持久化后重启可从上次断点继续（Task 5"Collector 断点"）。
//
// 存储格式：JSON {"seen":{"job-name":build_number}}。原子写入：先写同目录临时
// 文件再 os.Rename，避免进程在写一半时崩溃留下半截 JSON；Load 遇到损坏文件
// 返回错误，由调用方决定降级策略（本实现选择降级为空状态 + 告警日志，
// 最多重复发送一轮，靠 Exporter 幂等兜底，不会丢数据）。
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State 是持久化的断点数据。
type State struct {
	Seen map[string]int64 `json:"seen"`
}

// Load 从 path 读取断点。文件不存在时返回空的可用 State（首次启动）。
func Load(path string) (State, error) {
	s := State{Seen: make(map[string]int64)}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read state %q: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse state %q: %w", path, err)
	}
	if s.Seen == nil {
		s.Seen = make(map[string]int64)
	}
	return s, nil
}

// Save 原子写入断点到 path。自动创建父目录。
func Save(path string, s State) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// 同目录临时文件 + rename：保证 rename 前读者看到旧文件、之后看到完整新文件。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename state file: %w", err)
	}
	return nil
}
