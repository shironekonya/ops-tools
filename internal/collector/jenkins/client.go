// Package jenkins 是 Jenkins REST API 客户端。
//
// 职责边界：只负责"取回结构化构建信息 + 取回 console log 文本"，
// 不做日志内容解析（那是 internal/parser 的职责），也不做状态映射（那是 internal/model 的职责）。
//
// 认证：HTTP Basic Auth（用户名 + API Token），信息由 config 注入，不在此硬编码。
package jenkins

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ops-exporter/internal/collector/model"
)

// Client 访问 Jenkins REST API。
type Client struct {
	baseURL    string
	username   string
	apiToken   string
	httpClient *http.Client
}

// NewClient 创建客户端。timeout 是单次请求超时（由 config 注入）。
func NewClient(baseURL, username, apiToken string, timeout time.Duration) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		username:   username,
		apiToken:   apiToken,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// completedBuildResponse 是 /job/{name}/lastCompletedBuild/api/json 的结构化响应。
// 用指针接收 result 作为防御：lastCompletedBuild 正常情况下 result 非 null，
// 但 Jenkins 版本/权限异常时仍可能返回 null，此时按"未完成"跳过更安全。
type completedBuildResponse struct {
	Number    int64   `json:"number"`
	Result    *string `json:"result"`
	Timestamp int64   `json:"timestamp"` // 开始时间，毫秒
	Duration  int64   `json:"duration"`  // 耗时，毫秒
}

// GetLatestBuild 取某个 Job 最近一次已完成构建的信息。
//
// 刻意使用 lastCompletedBuild 而非 lastBuild：lastBuild 在 Jenkins 有构建正在
// 执行时 result 为 null，采集器会一直跳过——即使上一轮已完成构建有完整数据，
// 也会在 Job 长时间运行/频繁触发期间完全没有指标（现场实测过这个问题）。
// lastCompletedBuild 始终指向最后一个有结果的构建，保证"最近一次构建状态"
// 这类 Gauge 能持续反映已完成的事实。
//
// ok=false 仅在 result 异常为 null 时出现（防御性），调用方应跳过。
func (c *Client) GetLatestBuild(job string) (info model.BuildInfo, ok bool, err error) {
	path := "/job/" + url.PathEscape(job) + "/lastCompletedBuild/api/json"
	var resp completedBuildResponse
	if err := c.getJSON(path, &resp); err != nil {
		return model.BuildInfo{}, false, err
	}
	if resp.Result == nil {
		// 防御：lastCompletedBuild 理论上总有结果，null 时跳过而非猜状态。
		return model.BuildInfo{}, false, nil
	}
	return model.BuildInfo{
		JobName:     job,
		Number:      resp.Number,
		Result:      *resp.Result,
		TimestampMs: resp.Timestamp,
		DurationMs:  resp.Duration,
	}, true, nil
}

// GetConsoleLog 取指定构建的 console log 纯文本。
// number<=0 时返回错误（避免拼出异常路径）。
func (c *Client) GetConsoleLog(job string, number int64) (string, error) {
	if number <= 0 {
		return "", fmt.Errorf("invalid build number %d", number)
	}
	path := "/job/" + url.PathEscape(job) + "/" + fmt.Sprintf("%d", number) + "/consoleText"
	body, err := c.getRaw(path)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// getJSON 发起 GET 并解析 JSON。
func (c *Client) getJSON(path string, out any) error {
	body, err := c.getRaw(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse Jenkins response %q: %w", path, err)
	}
	return nil
}

// getRaw 发起带认证的 GET，检查状态码，返回响应体字节。
func (c *Client) getRaw(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %q: %w", path, err)
	}
	if c.username != "" || c.apiToken != "" {
		req.SetBasicAuth(c.username, c.apiToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %q: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 只读取少量响应体用于错误提示，避免把大页面读进内存。
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %q: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response %q: %w", path, err)
	}
	return body, nil
}
