package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvJenkinsUsername, EnvJenkinsToken} {
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unsetenv %s: %v", k, err)
		}
	}
}

// TestLoadConfig_Full 验证全量配置生效（认证来自文件）。
func TestLoadConfig_Full(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{
		"jenkins_url": "http://jenkins:8080",
		"username": "admin",
		"api_token": "tok123",
		"jobs": ["job-a", "job-b"],
		"exporter_url": "http://exporter:9105/api/v1/ingest/build",
		"interval": "15s",
		"timeout": "5s",
		"max_retries": 2,
		"state_file": "/var/lib/collector/state.json",
		"resend_interval": "3m"
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JenkinsURL != "http://jenkins:8080" || cfg.Username != "admin" || cfg.APIToken != "tok123" {
		t.Errorf("auth/url fields wrong: %+v", cfg)
	}
	if len(cfg.Jobs) != 2 || cfg.Jobs[0] != "job-a" || cfg.Jobs[1] != "job-b" {
		t.Errorf("Jobs = %v, want [job-a job-b]", cfg.Jobs)
	}
	if cfg.Interval != 15*time.Second || cfg.Timeout != 5*time.Second || cfg.MaxRetries != 2 {
		t.Errorf("timing fields wrong: %+v", cfg)
	}
	if cfg.StateFile != "/var/lib/collector/state.json" {
		t.Errorf("StateFile = %q", cfg.StateFile)
	}
	if cfg.ResendInterval != 3*time.Minute {
		t.Errorf("ResendInterval = %s, want 3m", cfg.ResendInterval)
	}
}

// TestLoadConfig_ResendInterval 心跳重发的默认/显式禁用/负值非法。
func TestLoadConfig_ResendInterval(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		name string
		raw  string // 配置片段中的 resend_interval 字段，空串表示不写该字段
		want time.Duration
		err  string
	}{
		{"未配置走默认2m", "", defaultResendInterval, ""},
		{"显式0s禁用", `"resend_interval":"0s"`, 0, ""},
		{"显式空串禁用", `"resend_interval":""`, 0, ""},
		{"自定义", `"resend_interval":"45s"`, 45 * time.Second, ""},
		{"负数非法", `"resend_interval":"-1s"`, 0, "must be non-negative"},
		{"格式非法", `"resend_interval":"abc"`, 0, "invalid resend_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := ""
			if tc.raw != "" {
				extra = "," + tc.raw
			}
			path := writeConfig(t, `{"jenkins_url":"http://j","jobs":["a"],"exporter_url":"http://x"`+extra+`}`)
			cfg, err := LoadConfig(path)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want error containing %q, got %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ResendInterval != tc.want {
				t.Errorf("ResendInterval = %s, want %s", cfg.ResendInterval, tc.want)
			}
		})
	}
}

// TestLoadConfig_EnvOverridesFile 环境变量覆盖文件里的认证信息，且不硬编码。
func TestLoadConfig_EnvOverridesFile(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvJenkinsUsername, "env-user")
	t.Setenv(EnvJenkinsToken, "env-token")
	path := writeConfig(t, `{
		"jenkins_url": "http://jenkins:8080",
		"username": "file-user",
		"api_token": "file-token",
		"jobs": ["job-a"],
		"exporter_url": "http://exporter:9105/api/v1/ingest/build"
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Username != "env-user" || cfg.APIToken != "env-token" {
		t.Errorf("env should override file: username=%q token=%q", cfg.Username, cfg.APIToken)
	}
}

// TestLoadConfig_Defaults 可选字段缺省走默认值。
func TestLoadConfig_Defaults(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `{
		"jenkins_url": "http://jenkins:8080",
		"jobs": ["job-a"],
		"exporter_url": "http://exporter:9105/api/v1/ingest/build"
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Interval != defaultInterval || cfg.Timeout != defaultTimeout || cfg.MaxRetries != defaultMaxRetries {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.ResendInterval != defaultResendInterval {
		t.Errorf("ResendInterval = %s, want default %s", cfg.ResendInterval, defaultResendInterval)
	}
}

// TestLoadConfig_Invalid 非法配置与必填项缺失必须报错。
func TestLoadConfig_Invalid(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"JSON 语法错误", `{bad`, "parse config"},
		{"缺 jenkins_url", `{"jobs":["a"],"exporter_url":"http://x"}`, "jenkins_url is required"},
		{"缺 jobs", `{"jenkins_url":"http://j","exporter_url":"http://x"}`, "jobs must contain"},
		{"jobs 含空串", `{"jenkins_url":"http://j","jobs":[""],"exporter_url":"http://x"}`, "jobs[0]"},
		{"缺 exporter_url", `{"jenkins_url":"http://j","jobs":["a"]}`, "exporter_url is required"},
		{"interval 非法", `{"jenkins_url":"http://j","jobs":["a"],"exporter_url":"http://x","interval":"abc"}`, "invalid interval"},
		{"interval 为零", `{"jenkins_url":"http://j","jobs":["a"],"exporter_url":"http://x","interval":"0s"}`, "interval must be positive"},
		{"timeout 为负", `{"jenkins_url":"http://j","jobs":["a"],"exporter_url":"http://x","timeout":"-1s"}`, "timeout must be positive"},
		{"max_retries 为负", `{"jenkins_url":"http://j","jobs":["a"],"exporter_url":"http://x","max_retries":-1}`, "max_retries must be non-negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.content))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoadConfig_MissingFile 文件不存在时只保留默认值，必填项仍校验。
func TestLoadConfig_MissingFile(t *testing.T) {
	clearEnv(t)
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil || !strings.Contains(err.Error(), "jenkins_url is required") {
		t.Fatalf("expected jenkins_url required error, got %v", err)
	}
}
