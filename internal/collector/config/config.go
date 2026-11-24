// Package config 加载并校验 Jenkins Collector 的运行时配置。
//
// 设计对齐 exporter 的 config.go：选 JSON（标准库，不引依赖），文件缺失走默认值，
// 非法字段致命报错。认证信息（用户名/API Token）支持配置文件与环境变量两种来源，
// 环境变量优先——这样生产环境可以把密钥放在环境变量/Secret 里，不落盘、不硬编码。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// 环境变量名：认证信息的推荐来源（CI/Secret 环境用）。
const (
	EnvJenkinsUsername = "JENKINS_USERNAME"
	EnvJenkinsToken    = "JENKINS_API_TOKEN"
)

// 默认值集中定义。
const (
	defaultInterval   = 30 * time.Second
	defaultTimeout    = 10 * time.Second
	defaultMaxRetries = 3
	// defaultResendInterval 心跳重发周期：Exporter 重启后内存清空，Collector 靠断点
	// 不会重发旧 build，Gauge 将永久 No data。2 分钟是"够快恢复 + 不产生无谓流量"的折中。
	// 设为 <=0 可禁用（保持旧行为）。
	defaultResendInterval = 2 * time.Minute
)

// Config 是加载并校验后的强类型配置。
type Config struct {
	JenkinsURL     string        // Jenkins 根地址，如 http://jenkins:8080
	Username       string        // Jenkins 用户名（可来自环境变量）
	APIToken       string        // Jenkins API Token（可来自环境变量，不硬编码）
	Jobs           []string      // 要采集的 Job 名列表（多个）
	ExporterURL    string        // exporter ingest 地址，如 http://exporter:9105/api/v1/ingest/build
	Interval       time.Duration // 采集间隔
	Timeout        time.Duration // 单次 HTTP 请求超时
	MaxRetries     int           // 发送失败重试次数（有限）
	StateFile      string        // 断点持久化路径（非空启用；Task 5）
	ResendInterval time.Duration // 心跳重发周期（<=0 禁用）; Exporter 重启后恢复 Gauge
}

// configFile 是 JSON 原始形态，间隔字段用字符串（time.ParseDuration）。
type configFile struct {
	JenkinsURL     string   `json:"jenkins_url"`
	Username       string   `json:"username"`
	APIToken       string   `json:"api_token"`
	Jobs           []string `json:"jobs"`
	ExporterURL    string   `json:"exporter_url"`
	Interval       string   `json:"interval"`
	Timeout        string   `json:"timeout"`
	MaxRetries     *int     `json:"max_retries"`
	StateFile      string   `json:"state_file"`
	ResendInterval *string  `json:"resend_interval"` // 指针区分"未配置"(默认2m)与"显式0/空"(禁用)
}

// LoadConfig 从 path 加载配置，再用环境变量覆盖认证字段。
// path 为空或文件不存在时只保留默认值 + 环境变量，必填项在最后统一校验。
func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Interval:       defaultInterval,
		Timeout:        defaultTimeout,
		MaxRetries:     defaultMaxRetries,
		ResendInterval: defaultResendInterval,
	}

	var f configFile
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		if err == nil {
			if err := json.Unmarshal(data, &f); err != nil {
				return Config{}, fmt.Errorf("parse config %q: %w", path, err)
			}
		}
	}

	// 基础字段：文件里写了才覆盖默认值。
	cfg.JenkinsURL = strings.TrimSpace(f.JenkinsURL)
	cfg.Username = strings.TrimSpace(f.Username)
	cfg.APIToken = strings.TrimSpace(f.APIToken)
	cfg.Jobs = f.Jobs
	cfg.ExporterURL = strings.TrimSpace(f.ExporterURL)
	cfg.StateFile = strings.TrimSpace(f.StateFile)

	if f.Interval != "" {
		d, err := time.ParseDuration(f.Interval)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid interval %q: %w", f.Interval, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("config: interval must be positive, got %q", f.Interval)
		}
		cfg.Interval = d
	}
	if f.Timeout != "" {
		d, err := time.ParseDuration(f.Timeout)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid timeout %q: %w", f.Timeout, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("config: timeout must be positive, got %q", f.Timeout)
		}
		cfg.Timeout = d
	}
	if f.MaxRetries != nil {
		if *f.MaxRetries < 0 {
			return Config{}, fmt.Errorf("config: max_retries must be non-negative, got %d", *f.MaxRetries)
		}
		cfg.MaxRetries = *f.MaxRetries
	}
	// resend_interval 可选：未配置走默认；显式 "0s"/空串禁用；负数非法。
	if f.ResendInterval != nil {
		v := strings.TrimSpace(*f.ResendInterval)
		if v == "" {
			cfg.ResendInterval = 0
		} else {
			d, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("config: invalid resend_interval %q: %w", v, err)
			}
			if d < 0 {
				return Config{}, fmt.Errorf("config: resend_interval must be non-negative, got %q", v)
			}
			cfg.ResendInterval = d
		}
	}

	// 环境变量覆盖认证信息（优先级高于配置文件）。
	if v := strings.TrimSpace(os.Getenv(EnvJenkinsUsername)); v != "" {
		cfg.Username = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvJenkinsToken)); v != "" {
		cfg.APIToken = v
	}

	// 必填项统一校验：collector 没有这些配置无法工作。
	if cfg.JenkinsURL == "" {
		return Config{}, fmt.Errorf("config: jenkins_url is required")
	}
	if len(cfg.Jobs) == 0 {
		return Config{}, fmt.Errorf("config: jobs must contain at least one job")
	}
	if cfg.ExporterURL == "" {
		return Config{}, fmt.Errorf("config: exporter_url is required")
	}
	for i, job := range cfg.Jobs {
		if strings.TrimSpace(job) == "" {
			return Config{}, fmt.Errorf("config: jobs[%d] must not be empty", i)
		}
	}

	return cfg, nil
}
