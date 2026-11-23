// Package sender 把 BuildResult 通过 HTTP POST 发送给 Custom Exporter。
//
// 职责：请求超时、状态码检查、失败记录、有限次数重试。
// 对接契约见 internal/model.BuildResult（与 Exporter build.go 同构）与
// Exporter 的 POST /api/v1/ingest/build。
package sender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"ops-exporter/internal/collector/model"
)

// Sender 负责发送构建结果。
type Sender struct {
	endpoint   string
	httpClient *http.Client
	maxRetries int
	logger     *slog.Logger
}

// NewSender 创建发送器。
// timeout 是单次请求超时；maxRetries 是失败后的额外尝试次数（0 表示只发一次）。
func NewSender(endpoint string, timeout time.Duration, maxRetries int, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sender{
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: timeout},
		maxRetries: maxRetries,
		logger:     logger,
	}
}

// Send 发送一条构建结果，成功返回 nil，彻底失败返回最后一次错误。
//
// 重试策略：只对"可重试"错误重试——网络错误、5xx、429（服务端临时不可用）。
// 4xx 其他状态码（如 400 数据非法）重试没有意义，直接失败，避免刷屏。
// Exporter 对重复/旧 build 返回 200 + status=ignored，本函数视为成功（幂等）。
func (s *Sender) Send(build model.BuildResult) error {
	body, err := json.Marshal(build)
	if err != nil {
		return fmt.Errorf("marshal build %s#%d: %w", build.JobName, build.BuildNumber, err)
	}

	var lastErr error
	attempts := s.maxRetries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		status, respBody, err := s.post(body)
		if err == nil && status >= 200 && status < 300 {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d: %s", status, string(respBody))
		}

		if !retryable(status, err) || attempt == attempts {
			break
		}
		s.logger.Warn("send build failed, retrying",
			"job", build.JobName,
			"build_number", build.BuildNumber,
			"attempt", attempt,
			"max_attempts", attempts,
			"error", lastErr.Error())
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond) // 简单线性退避
	}

	s.logger.Error("send build failed",
		"job", build.JobName,
		"build_number", build.BuildNumber,
		"attempts", attempts,
		"error", lastErr.Error())
	return lastErr
}

// post 执行一次 POST，返回状态码、响应体（截断到 512 字节）与错误。
func (s *Sender) post(body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST %q: %w", s.endpoint, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// retryable 判断错误是否值得重试。
func retryable(status int, err error) bool {
	if err != nil {
		return true // 网络错误可重试
	}
	return status == http.StatusTooManyRequests || status >= 500
}
