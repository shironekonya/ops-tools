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
	builds         buildProvider
	logs           logProvider
	sender         buildSender
	jobs           []string
	logger         *slog.Logger
	stateFile      string        // 非空时把 seen 持久化到该路径（断点续传，Task 5）
	resendInterval time.Duration // >0 时按此周期重发各 job 最新 Build（心跳重发）；<=0 禁用

	mu           sync.Mutex
	seen         map[string]int64             // job -> 上次成功处理的 build_number（去重）
	latestBuild  map[string]model.BuildResult // job -> 最近一次成功发送的 Build（心跳重发用）
	lastResendAt map[string]time.Time         // job -> 上次实际发送时刻（心跳计时基准）
	now          func() time.Time             // 可注入时钟，便于测试；默认 time.Now
}

// New 创建采集器。
func New(client *jenkins.Client, s *sender.Sender, jobs []string, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.Default()
	}
	cp := make([]string, len(jobs))
	copy(cp, jobs)
	return &Collector{
		builds:       client,
		logs:         client, // 默认用同一个 client 取日志
		sender:       s,
		jobs:         cp,
		logger:       logger,
		seen:         make(map[string]int64),
		latestBuild:  make(map[string]model.BuildResult),
		lastResendAt: make(map[string]time.Time),
		now:          time.Now,
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
		jobs:         append([]string(nil), jobs...),
		logger:       logger,
		seen:         make(map[string]int64),
		latestBuild:  make(map[string]model.BuildResult),
		lastResendAt: make(map[string]time.Time),
		now:          time.Now,
	}
}

// CollectOnce 执行一轮采集，返回成功发送的数量与发生的错误数。
//
// 去重规则：只处理 build_number 严格大于 seen[job] 的构建；
// 且只有发送成功才更新 seen——这样发送失败时下一轮会重试，不会永久丢失。
//
// 心跳重发（resendInterval>0）：Exporter 是内存存储，重启后 BuildStore 清空，
// 而 Collector 断点记着"这个 build 已处理"就不会再发，导致 jenkins_build_*
// 的 Gauge 永久停在 No data（Counter 因 Prometheus 侧历史仍在而看似正常）。
// 因此对已去重跳过的 job，若距上次实际发送超过 resendInterval，则重发其最新
// Build：Exporter 正常时用 build_number 幂等返回 ignored 不重复计数；重启后
// 内存为空则接受并重建 Gauge。重发不更新 seen，不改断点续传语义。
//
// 关键：重发不依赖内存缓存。Collector 自身也可能重启（latestBuild 丢失），
// 此时从 Jenkins 现场重新构造 Build（与首次采集同一路径），保证任何重启顺序
// 下都能恢复。缓存仅作优化：命中且 build_number 一致时省去重复拉 console log。
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
			// 已是处理过的 build：到达心跳周期则重发。
			if c.resendInterval > 0 && c.dueForResend(job) {
				s, e := c.resend(job, info)
				sent += s
				errs += e
			}
			continue
		}

		build, ok := c.buildFromInfo(job, info)
		if !ok {
			continue
		}
		if err := c.sender.Send(build); err != nil {
			c.logger.Error("send build failed", "job", job, "build_number", info.Number, "error", err)
			errs++
			continue // 不更新 seen，下轮重试
		}

		c.markProcessed(job, info.Number)
		c.rememberSent(job, build) // 缓存 + 重置心跳计时
		sent++
		c.logger.Info("build collected", "job", job, "build_number", info.Number, "status", build.Status)
	}
	return sent, errs
}

// buildFromInfo 从 Jenkins 构建信息 + console log 构造标准化 Build。
// 正常采集与心跳重发共用，保证两条路径产出的指标完全一致。
func (c *Collector) buildFromInfo(job string, info model.BuildInfo) (model.BuildResult, bool) {
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
	}
	return build, ok
}

// resend 心跳重发一次：优先用缓存（命中且 build_number 一致），否则现场重建。
// 返回 (成功数, 错误数)。发送失败不推进计时，下一轮立即重试。
func (c *Collector) resend(job string, info model.BuildInfo) (sent int, errs int) {
	build, cached := c.getLatestBuild(job)
	if !cached || build.BuildNumber != info.Number {
		var ok bool
		build, ok = c.buildFromInfo(job, info)
		if !ok {
			return 0, 0
		}
	}
	if err := c.sender.Send(build); err != nil {
		c.logger.Error("heartbeat resend failed", "job", job, "build_number", build.BuildNumber, "error", err)
		return 0, 1
	}
	c.rememberSent(job, build) // 重置计时并更新缓存，但不触碰 seen
	c.logger.Info("heartbeat resent", "job", job, "build_number", build.BuildNumber)
	return 1, 0
}

// dueForResend 判断某 job 是否到达心跳重发时刻。
// 本进程从未发过（无计时基准）时：若断点里有该 job（从 state 恢复，说明历史上
// 成功发过），立即返回 true 补一次——这覆盖"Exporter 与 Collector 同时重启"
// 场景：Collector 无缓存、断点认为已处理，若不补发 Gauge 将永久 No data。
func (c *Collector) dueForResend(job string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, hasTimer := c.lastResendAt[job]
	if !hasTimer {
		_, seenBefore := c.seen[job]
		return seenBefore
	}
	return c.now().Sub(last) >= c.resendInterval
}

// getLatestBuild 返回缓存的最近一次成功发送的 Build（副本）。
func (c *Collector) getLatestBuild(job string) (model.BuildResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.latestBuild[job]
	return b, ok
}

// rememberSent 在成功发送后缓存 Build 并重置心跳计时。
func (c *Collector) rememberSent(job string, build model.BuildResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latestBuild[job] = build
	c.lastResendAt[job] = c.now()
}

// SetResendInterval 配置心跳重发周期（<=0 禁用）。
// 用 setter 而非构造参数，保持 New / NewWithState 的既有签名不变（向后兼容）。
func (c *Collector) SetResendInterval(d time.Duration) {
	c.resendInterval = d
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
