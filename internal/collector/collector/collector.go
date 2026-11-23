// Package collector 编排定时采集：ticker 驱动，对每个 Job 取最新 Build →
// 解析日志 → 标准化 → 发送，并维护去重状态避免重复处理同一 Build。
package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"ops-exporter/internal/collector/jenkins"
	"ops-exporter/internal/collector/model"
	"ops-exporter/internal/collector/parser"
	"ops-exporter/internal/collector/sender"
	"ops-exporter/internal/collector/state"
)

// 三个窄接口把采集编排与具体实现解耦，测试可注入 mock 而不起 HTTP 服务。
// jenkins.Client 同时满足 buildProvider + logProvider；sender.Sender 满足 buildSender。
type buildProvider interface {
	GetLatestBuild(job string) (info model.BuildInfo, ok bool, err error)
}

type logProvider interface {
	GetConsoleLog(job string, number int64) (string, error)
}

type buildSender interface {
	Send(build model.BuildResult) error
}

// Collector 定时采集器。
type Collector struct {
	builds    buildProvider
	logs      logProvider
	sender    buildSender
	jobs      []string
	logger    *slog.Logger
	stateFile string // 非空时把 seen 持久化到该路径（断点续传，Task 5）

	mu   sync.Mutex
	seen map[string]int64 // job -> 上次成功处理的 build_number（去重）
}

// New 创建采集器。
func New(client *jenkins.Client, s *sender.Sender, jobs []string, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.Default()
	}
	cp := make([]string, len(jobs))
	copy(cp, jobs)
	return &Collector{
		builds: client,
		logs:   client, // 默认用同一个 client 取日志
		sender: s,
		jobs:   cp,
		logger: logger,
		seen:   make(map[string]int64),
	}
}

// NewWithState 创建采集器并从 stateFile 恢复断点（Task 5）。
// 文件不存在首次启动属正常；文件损坏则降级为空状态并告警——最坏重复发送一轮，
// 由 Exporter 的幂等去重兜底，不会丢数据。
func NewWithState(client *jenkins.Client, s *sender.Sender, jobs []string, logger *slog.Logger, stateFile string) *Collector {
	c := New(client, s, jobs, logger)
	c.stateFile = stateFile
	st, err := state.Load(stateFile)
	if err != nil {
		c.logger.Warn("load state failed, starting from empty", "file", stateFile, "error", err)
		return c
	}
	c.seen = st.Seen
	if len(st.Seen) > 0 {
		c.logger.Info("state restored", "file", stateFile, "jobs", len(st.Seen))
	}
	return c
}

// newWithDeps 用注入的依赖创建采集器（测试用，同包内调用）。
func newWithDeps(b buildProvider, l logProvider, s buildSender, jobs []string, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.Default()
	}
	return &Collector{
		builds: b, logs: l, sender: s,
		jobs:   append([]string(nil), jobs...),
		logger: logger, seen: make(map[string]int64),
	}
}

// CollectOnce 执行一轮采集，返回成功发送的数量与发生的错误数。
//
// 去重规则：只处理 build_number 严格大于 seen[job] 的构建；
// 且只有发送成功才更新 seen——这样发送失败时下一轮会重试，不会永久丢失。
func (c *Collector) CollectOnce() (sent int, errs int) {
	for _, job := range c.jobs {
		info, ok, err := c.builds.GetLatestBuild(job)
		if err != nil {
			c.logger.Error("get latest build failed", "job", job, "error", err)
			errs++
			continue
		}
		if !ok {
			// 构建尚未完成，跳过。
			continue
		}

		if !c.shouldProcess(job, info.Number) {
			continue
		}

		// 先取日志解析出失败阶段（解析失败不影响主流程，降级为空阶段）。
		failedStage := ""
		logText, err := c.logs.GetConsoleLog(job, info.Number)
		if err != nil {
			c.logger.Warn("get console log failed, continue without stage", "job", job, "build_number", info.Number, "error", err)
		} else {
			failedStage = parser.Parse(logText).FailedStage
		}

		build, ok := model.ToBuildResult(info, failedStage)
		if !ok {
			c.logger.Warn("skip build with unknown result", "job", job, "build_number", info.Number, "result", info.Result)
			continue
		}

		if err := c.sender.Send(build); err != nil {
			c.logger.Error("send build failed", "job", job, "build_number", info.Number, "error", err)
			errs++
			continue // 不更新 seen，下轮重试
		}

		c.markProcessed(job, info.Number)
		sent++
		c.logger.Info("build collected", "job", job, "build_number", info.Number, "status", build.Status)
	}
	return sent, errs
}

// shouldProcess 判断 build 是否比已处理的更新。
func (c *Collector) shouldProcess(job string, number int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, seen := c.seen[job]
	return !seen || number > prev
}

// markProcessed 记录已成功处理的 build，并在配置 stateFile 时持久化断点。
func (c *Collector) markProcessed(job string, number int64) {
	c.mu.Lock()
	prev, seen := c.seen[job]
	if !seen || number > prev {
		c.seen[job] = number
	}
	// 持锁构造快照，避免把锁范围扩大到磁盘 IO。
	snapshot := make(map[string]int64, len(c.seen))
	for k, v := range c.seen {
		snapshot[k] = v
	}
	c.mu.Unlock()

	if c.stateFile != "" {
		if err := state.Save(c.stateFile, state.State{Seen: snapshot}); err != nil {
			// 保存失败不回滚 seen：Build 已成功发送，最坏重启后重复一次（幂等兜底）。
			c.logger.Warn("save state failed", "file", c.stateFile, "error", err)
		}
	}
}

// Run 启动采集循环：立即采一次，之后按 interval 周期采集，ctx 取消时退出。
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	c.CollectOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("collector stopped")
			return
		case <-ticker.C:
			c.CollectOnce()
		}
	}
}
