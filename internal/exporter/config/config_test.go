package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig 在临时目录写一个配置文件，返回完整路径。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestLoadConfig_MissingFile 验收：文件不存在不算错误，返回纯默认值。
// 首次部署允许零配置启动。
func TestLoadConfig_MissingFile(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if cfg.ListenAddr != defaultListenAddr || cfg.ResultDir != defaultResultDir {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.RefreshInterval != defaultRefreshInterval {
		t.Errorf("RefreshInterval = %v, want %v", cfg.RefreshInterval, defaultRefreshInterval)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.MaxIngestBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxIngestBodyBytes = %d, want %d", cfg.MaxIngestBodyBytes, DefaultMaxBodyBytes)
	}
	// Task 5：HTTP 超时默认值必须存在（防 Slowloris）。
	if cfg.ReadHeaderTimeout != defaultReadHeaderTimeout || cfg.ReadTimeout != defaultReadTimeout ||
		cfg.WriteTimeout != defaultWriteTimeout || cfg.IdleTimeout != defaultIdleTimeout ||
		cfg.ShutdownTimeout != defaultShutdownTimeout || cfg.MaxHeaderBytes != defaultMaxHeaderBytes {
		t.Errorf("http timeout defaults not applied: %+v", cfg)
	}
}

// TestLoadConfig_EmptyPath 验证不指定配置文件时直接返回默认值。
func TestLoadConfig_EmptyPath(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
}

// TestLoadConfig_Full 验证四项配置全部生效。
func TestLoadConfig_Full(t *testing.T) {
	path := writeConfig(t, `{
		"listen_addr": ":9999",
		"result_dir": "/var/ops-results",
		"refresh_interval": "30s",
		"log_level": "debug"
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999", cfg.ListenAddr)
	}
	if cfg.ResultDir != "/var/ops-results" {
		t.Errorf("ResultDir = %q", cfg.ResultDir)
	}
	if cfg.RefreshInterval != 30*time.Second {
		t.Errorf("RefreshInterval = %v, want 30s", cfg.RefreshInterval)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
}

// TestLoadConfig_Partial 验证部分配置：没写的字段回落到默认值。
func TestLoadConfig_Partial(t *testing.T) {
	path := writeConfig(t, `{"refresh_interval": "1m"}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RefreshInterval != time.Minute {
		t.Errorf("RefreshInterval = %v, want 1m", cfg.RefreshInterval)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want default %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want default info", cfg.LogLevel)
	}
}

// TestLoadConfig_IngestSection 验证可选 ingest 段：写了生效，没写走默认（向后兼容）。
func TestLoadConfig_IngestSection(t *testing.T) {
	withIngest, err := LoadConfig(writeConfig(t, `{"ingest": {"max_body_bytes": 4096}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if withIngest.MaxIngestBodyBytes != 4096 {
		t.Errorf("MaxIngestBodyBytes = %d, want 4096", withIngest.MaxIngestBodyBytes)
	}

	withoutIngest, err := LoadConfig(writeConfig(t, `{"listen_addr": ":9105"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if withoutIngest.MaxIngestBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxIngestBodyBytes = %d, want default %d", withoutIngest.MaxIngestBodyBytes, DefaultMaxBodyBytes)
	}
}

// TestLoadConfig_HTTPSection 验证可选 http 段：写了生效，没写走默认（向后兼容）。
func TestLoadConfig_HTTPSection(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{
		"http": {
			"read_header_timeout": "2s",
			"read_timeout": "15s",
			"write_timeout": "20s",
			"idle_timeout": "45s",
			"shutdown_timeout": "8s",
			"max_header_bytes": 2048
		}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"read_header_timeout", cfg.ReadHeaderTimeout, 2 * time.Second},
		{"read_timeout", cfg.ReadTimeout, 15 * time.Second},
		{"write_timeout", cfg.WriteTimeout, 20 * time.Second},
		{"idle_timeout", cfg.IdleTimeout, 45 * time.Second},
		{"shutdown_timeout", cfg.ShutdownTimeout, 8 * time.Second},
		{"max_header_bytes", cfg.MaxHeaderBytes, 2048},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// 部分覆盖：只写一项，其余保持默认。
	partial, err := LoadConfig(writeConfig(t, `{"http": {"read_header_timeout": "3s"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if partial.ReadHeaderTimeout != 3*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 3s", partial.ReadHeaderTimeout)
	}
	if partial.WriteTimeout != defaultWriteTimeout {
		t.Errorf("WriteTimeout = %v, want default %v", partial.WriteTimeout, defaultWriteTimeout)
	}
}

// TestLoadConfig_Expiry 验证数据过期配置：默认禁用；显式启用/禁用/非法值。
func TestLoadConfig_Expiry(t *testing.T) {
	// 默认禁用，但扫描间隔有默认值。
	def, err := LoadConfig(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.MaxBuildAge != 0 {
		t.Errorf("MaxBuildAge = %v, want 0 (disabled by default)", def.MaxBuildAge)
	}
	if def.ExpiryInterval != defaultExpiryInterval {
		t.Errorf("ExpiryInterval = %v, want %v", def.ExpiryInterval, defaultExpiryInterval)
	}

	// 显式启用。
	on, err := LoadConfig(writeConfig(t, `{"ingest": {"max_build_age": "24h", "expiry_interval": "1m"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if on.MaxBuildAge != 24*time.Hour || on.ExpiryInterval != time.Minute {
		t.Errorf("expiry config = %v / %v, want 24h / 1m", on.MaxBuildAge, on.ExpiryInterval)
	}

	// 显式 "0s" 也是合法禁用。
	off, err := LoadConfig(writeConfig(t, `{"ingest": {"max_build_age": "0s"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if off.MaxBuildAge != 0 {
		t.Errorf("MaxBuildAge = %v, want 0", off.MaxBuildAge)
	}
}

// TestLoadConfig_LevelCaseInsensitive 验证日志级别大小写不敏感。
func TestLoadConfig_LevelCaseInsensitive(t *testing.T) {
	path := writeConfig(t, `{"log_level": "WARN"}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want warn", cfg.LogLevel)
	}
}

// TestLoadConfig_Invalid 集中覆盖非法配置：必须报错且错误能定位到字段。
func TestLoadConfig_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"JSON 语法错误", `{bad json`, "parse config"},
		{"间隔格式错误", `{"refresh_interval": "abc"}`, "invalid refresh_interval"},
		{"间隔为零", `{"refresh_interval": "0s"}`, "refresh_interval must be positive"},
		{"间隔为负", `{"refresh_interval": "-5s"}`, "refresh_interval must be positive"},
		{"日志级别非法", `{"log_level": "loud"}`, "invalid log_level"},
		{"字段类型错误", `{"refresh_interval": 30}`, "parse config"},
		{"ingest 体积为零", `{"ingest": {"max_body_bytes": 0}}`, "ingest.max_body_bytes must be positive"},
		{"ingest 体积为负", `{"ingest": {"max_body_bytes": -1}}`, "ingest.max_body_bytes must be positive"},
		{"http 超时格式错误", `{"http": {"read_timeout": "abc"}}`, "invalid http.read_timeout"},
		{"http 超时为零", `{"http": {"write_timeout": "0s"}}`, "http.write_timeout must be positive"},
		{"http max_header_bytes 为负", `{"http": {"max_header_bytes": -1}}`, "http.max_header_bytes must be positive"},
		{"max_build_age 格式错误", `{"ingest": {"max_build_age": "abc"}}`, "invalid ingest.max_build_age"},
		{"max_build_age 为负", `{"ingest": {"max_build_age": "-1h"}}`, "max_build_age must not be negative"},
		{"expiry_interval 为零", `{"ingest": {"expiry_interval": "0s"}}`, "expiry_interval must be positive"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.content)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
