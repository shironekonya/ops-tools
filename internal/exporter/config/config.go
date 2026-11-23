package config

// 本文件实现 TODO 8：配置文件。
//
// 数据流：config.json --LoadConfig--> Config --main--> HTTP Server / 刷新循环 / 日志
//
// 设计约定：
//   - 选 JSON 而不是 YAML：标准库 encoding/json 即可，不引第三方依赖；
//   - 文件不存在（未配置过）→ 全部用默认值，首次部署零配置可跑；
//   - 文件存在但解析失败 / 字段非法 → 致命错误，拒绝启动。
//     带病运行（比如刷新间隔被写成 0）比不启动更危险；
//   - 文件里没写的字段 → 用默认值，支持只覆盖关心的项；
//   - 不引入 Viper 之类的配置框架，四个字段用不上。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// 默认值集中定义，LoadConfig 和文档共用这一份。
const (
	defaultListenAddr      = ":9105"
	defaultResultDir       = "data"
	defaultRefreshInterval = 10 * time.Second
	defaultLogLevel        = "info"
	// DefaultMaxBodyBytes 是 HTTP 接收接口允许的最大请求体（1MB）。
	// 一条 Build JSON 通常几百字节，1MB 足够宽松，同时防止异常大包打满内存。
	DefaultMaxBodyBytes = int64(1024 * 1024)

	// HTTP Server 超时默认值（Task 5：请求限制/超时完善）。
	// http.Server 零值没有读/写超时，恶意客户端可以保持连接不开口（Slowloris）
	// 或极慢地读响应来占用 goroutine；这组默认值堵住这两类资源耗尽攻击。
	// 抓取 /metrics 时文本可能较大，写超时给到 30s；空闲连接 60s 后回收。
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout   = 5 * time.Second
	defaultMaxHeaderBytes    = 1 << 20 // 1 MiB，与 Go 默认值一致，显式化便于配置

	// 数据过期默认禁用（MaxBuildAge=0）：不配置不会删除任何数据，向后兼容。
	// 需要启用时在 config.json 的 ingest 段设置 max_build_age（如 "24h"）。
	defaultExpiryInterval = 5 * time.Minute // 启用过期后的清理扫描间隔
)

// Config 是加载并校验后的运行时配置，字段全部为强类型。
type Config struct {
	ListenAddr      string        // HTTP 监听地址，如 ":9105"
	ResultDir       string        // 任务结果目录
	RefreshInterval time.Duration // 指标刷新间隔
	LogLevel        slog.Level    // 日志级别（debug/info/warn/error）

	// MaxIngestBodyBytes POST /api/v1/ingest/build 允许的最大请求体字节数。
	// 属于新增的可选配置段，缺省走 DefaultMaxBodyBytes，不破坏旧配置文件。
	MaxIngestBodyBytes int64

	// HTTP Server 超时（Task 5）。全部有默认值，可选 http 段覆盖。
	ReadHeaderTimeout time.Duration // 读请求头超时，防 Slowloris
	ReadTimeout       time.Duration // 读完整请求（含 body）超时
	WriteTimeout      time.Duration // 写响应超时
	IdleTimeout       time.Duration // keep-alive 空闲连接超时
	ShutdownTimeout   time.Duration // 优雅退出等待在途请求的最长时间
	MaxHeaderBytes    int           // 请求头最大字节数

	// 数据过期（Task 5）：MaxBuildAge<=0 表示禁用；否则清理超过该时长未更新的 Job。
	MaxBuildAge    time.Duration
	ExpiryInterval time.Duration // 过期扫描间隔
}

// configFile 是 JSON 文件的原始形态，字段全是字符串，
// 解析成 Config 之前逐项转换和校验，错误信息能精确指到字段。
type configFile struct {
	ListenAddr      string `json:"listen_addr"`
	ResultDir       string `json:"result_dir"`
	RefreshInterval string `json:"refresh_interval"`
	LogLevel        string `json:"log_level"`

	// Ingest 可选段：省略时为 nil，走默认值（向后兼容现有 config.json）。
	Ingest *ingestConfigFile `json:"ingest"`

	// HTTP 可选段：超时配置，省略时全部走默认值。
	HTTP *httpConfigFile `json:"http"`
}

// ingestConfigFile 对应 config.json 的 "ingest" 子对象。
// 用指针类型区分"没写"（nil→默认）和"写了 0"（非法，见 LoadConfig）。
type ingestConfigFile struct {
	MaxBodyBytes   *int64  `json:"max_body_bytes"`
	MaxBuildAge    *string `json:"max_build_age"`   // 如 "24h"；缺省或 "0s" 表示禁用过期
	ExpiryInterval *string `json:"expiry_interval"` // 清理扫描间隔，默认 5m
}

// httpConfigFile 对应 config.json 的 "http" 子对象。
// 时长字段是字符串（time.ParseDuration）；用指针区分"没写"和"写了非法值"。
type httpConfigFile struct {
	ReadHeaderTimeout *string `json:"read_header_timeout"`
	ReadTimeout       *string `json:"read_timeout"`
	WriteTimeout      *string `json:"write_timeout"`
	IdleTimeout       *string `json:"idle_timeout"`
	ShutdownTimeout   *string `json:"shutdown_timeout"`
	MaxHeaderBytes    *int    `json:"max_header_bytes"`
}

// LoadConfig 从 path 加载配置。
// path 为空或文件不存在时返回纯默认值；
// 文件内容非法时返回带字段名的错误。
func LoadConfig(path string) (Config, error) {
	cfg := Config{
		ListenAddr:         defaultListenAddr,
		ResultDir:          defaultResultDir,
		RefreshInterval:    defaultRefreshInterval,
		LogLevel:           slog.LevelInfo,
		MaxIngestBodyBytes: DefaultMaxBodyBytes,
		ReadHeaderTimeout:  defaultReadHeaderTimeout,
		ReadTimeout:        defaultReadTimeout,
		WriteTimeout:       defaultWriteTimeout,
		IdleTimeout:        defaultIdleTimeout,
		ShutdownTimeout:    defaultShutdownTimeout,
		MaxHeaderBytes:     defaultMaxHeaderBytes,
		ExpiryInterval:     defaultExpiryInterval,
	}

	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// 没有配置文件不算错误：首次部署允许零配置启动。
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	var f configFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	if f.ListenAddr != "" {
		cfg.ListenAddr = f.ListenAddr
	}
	if f.ResultDir != "" {
		cfg.ResultDir = f.ResultDir
	}
	if f.RefreshInterval != "" {
		d, err := time.ParseDuration(f.RefreshInterval)
		if err != nil {
			return Config{}, fmt.Errorf("config %q: invalid refresh_interval %q: %w", path, f.RefreshInterval, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("config %q: refresh_interval must be positive, got %q", path, f.RefreshInterval)
		}
		cfg.RefreshInterval = d
	}
	if f.LogLevel != "" {
		// slog.Level 自带 UnmarshalText，接受 debug/info/warn/error（大小写不敏感）。
		var level slog.Level
		if err := level.UnmarshalText([]byte(f.LogLevel)); err != nil {
			return Config{}, fmt.Errorf("config %q: invalid log_level %q: %w", path, f.LogLevel, err)
		}
		cfg.LogLevel = level
	}

	// 可选 ingest 段：没写走默认；写了 max_body_bytes 则要求为正数。
	if f.Ingest != nil && f.Ingest.MaxBodyBytes != nil {
		if *f.Ingest.MaxBodyBytes <= 0 {
			return Config{}, fmt.Errorf("config %q: ingest.max_body_bytes must be positive, got %d", path, *f.Ingest.MaxBodyBytes)
		}
		cfg.MaxIngestBodyBytes = *f.Ingest.MaxBodyBytes
	}
	// max_build_age 允许为 0（显式禁用过期），负数非法。
	if f.Ingest != nil && f.Ingest.MaxBuildAge != nil {
		d, err := time.ParseDuration(*f.Ingest.MaxBuildAge)
		if err != nil {
			return Config{}, fmt.Errorf("config %q: invalid ingest.max_build_age %q: %w", path, *f.Ingest.MaxBuildAge, err)
		}
		if d < 0 {
			return Config{}, fmt.Errorf("config %q: ingest.max_build_age must not be negative, got %q", path, *f.Ingest.MaxBuildAge)
		}
		cfg.MaxBuildAge = d
	}
	if f.Ingest != nil && f.Ingest.ExpiryInterval != nil {
		d, err := time.ParseDuration(*f.Ingest.ExpiryInterval)
		if err != nil {
			return Config{}, fmt.Errorf("config %q: invalid ingest.expiry_interval %q: %w", path, *f.Ingest.ExpiryInterval, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("config %q: ingest.expiry_interval must be positive, got %q", path, *f.Ingest.ExpiryInterval)
		}
		cfg.ExpiryInterval = d
	}

	// 可选 http 段：每项独立覆盖默认值，写了就要求为正数。
	if f.HTTP != nil {
		h := f.HTTP
		durations := []struct {
			name string
			raw  *string
			dst  *time.Duration
		}{
			{"read_header_timeout", h.ReadHeaderTimeout, &cfg.ReadHeaderTimeout},
			{"read_timeout", h.ReadTimeout, &cfg.ReadTimeout},
			{"write_timeout", h.WriteTimeout, &cfg.WriteTimeout},
			{"idle_timeout", h.IdleTimeout, &cfg.IdleTimeout},
			{"shutdown_timeout", h.ShutdownTimeout, &cfg.ShutdownTimeout},
		}
		for _, item := range durations {
			if item.raw == nil {
				continue
			}
			d, err := time.ParseDuration(*item.raw)
			if err != nil {
				return Config{}, fmt.Errorf("config %q: invalid http.%s %q: %w", path, item.name, *item.raw, err)
			}
			if d <= 0 {
				return Config{}, fmt.Errorf("config %q: http.%s must be positive, got %q", path, item.name, *item.raw)
			}
			*item.dst = d
		}
		if h.MaxHeaderBytes != nil {
			if *h.MaxHeaderBytes <= 0 {
				return Config{}, fmt.Errorf("config %q: http.max_header_bytes must be positive, got %d", path, *h.MaxHeaderBytes)
			}
			cfg.MaxHeaderBytes = *h.MaxHeaderBytes
		}
	}

	return cfg, nil
}
