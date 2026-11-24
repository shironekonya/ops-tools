package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ops-exporter/internal/collector/model"
	"ops-exporter/internal/collector/state"
)

// --- mocks ---

type mockBuilds struct {
	mu    sync.Mutex
	infos map[string]model.BuildInfo
	oks   map[string]bool
	errs  map[string]error
	calls map[string]int
}

func newMockBuilds() *mockBuilds {
	return &mockBuilds{infos: map[string]model.BuildInfo{}, oks: map[string]bool{}, errs: map[string]error{}, calls: map[string]int{}}
}
func (m *mockBuilds) GetLatestBuild(job string) (model.BuildInfo, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[job]++
	return m.infos[job], m.oks[job], m.errs[job]
}
func (m *mockBuilds) set(job string, info model.BuildInfo, ok bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.infos[job] = info
	m.oks[job] = ok
	m.errs[job] = err
}

type mockLogs struct {
	text string
	err  error
}

func (m *mockLogs) GetConsoleLog(job string, number int64) (string, error) { return m.text, m.err }

type mockSender struct {
	mu     sync.Mutex
	sent   []model.BuildResult
	failOn int64 // build_number == failOn 时返回错误，0 表示不失败
}

func (m *mockSender) Send(b model.BuildResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != 0 && b.BuildNumber == m.failOn {
		return errors.New("send failed")
	}
	m.sent = append(m.sent, b)
	return nil
}
func (m *mockSender) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func successInfo(job string, num int64) model.BuildInfo {
	return model.BuildInfo{JobName: job, Number: num, Result: model.JenkinsSuccess, TimestampMs: 1700000000000, DurationMs: 1000}
}

// TestCollectOnce_Sends 正常链路：取构建→解析日志→标准化→发送，阶段名带入。
func TestCollectOnce_Sends(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	logs := &mockLogs{text: "[Pipeline] Stage: Deploy\nERROR: failed"}
	s := &mockSender{}
	c := newWithDeps(b, logs, s, []string{"deploy"}, testLogger())

	sent, errs := c.CollectOnce()
	if sent != 1 || errs != 0 {
		t.Fatalf("sent=%d errs=%d, want 1/0", sent, errs)
	}
	if s.count() != 1 {
		t.Fatalf("sender calls = %d, want 1", s.count())
	}
	got := s.sent[0]
	if got.JobName != "deploy" || got.BuildNumber != 42 || got.Status != model.StatusSuccess {
		t.Errorf("sent build wrong: %+v", got)
	}
	if got.FailedStage != "Deploy" {
		t.Errorf("FailedStage = %q, want Deploy", got.FailedStage)
	}
}

// TestCollectOnce_Dedup 同一个 Build 不重复处理（去重）。
func TestCollectOnce_Dedup(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	c.CollectOnce()
	c.CollectOnce()
	c.CollectOnce()
	if s.count() != 1 {
		t.Errorf("sender calls = %d, want 1 (dedup)", s.count())
	}
}

// TestCollectOnce_NewBuild 新 build_number 触发再次发送。
func TestCollectOnce_NewBuild(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	c.CollectOnce()
	b.set("deploy", successInfo("deploy", 43), true, nil)
	c.CollectOnce()
	if s.count() != 2 {
		t.Errorf("sender calls = %d, want 2 for two builds", s.count())
	}
}

// TestCollectOnce_OlderBuildIgnored 更旧的 build（乱序）不处理。
func TestCollectOnce_OlderBuildIgnored(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 50), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	c.CollectOnce()
	b.set("deploy", successInfo("deploy", 49), true, nil)
	c.CollectOnce()
	if s.count() != 1 {
		t.Errorf("older build should not be sent, calls = %d", s.count())
	}
}

// TestCollectOnce_SendFailureRetriedNextRound 发送失败不更新 seen，下一轮重试。
func TestCollectOnce_SendFailureRetriedNextRound(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{failOn: 42}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	_, errs := c.CollectOnce()
	if errs != 1 || s.count() != 0 {
		t.Fatalf("first round errs=%d sent=%d", errs, s.count())
	}
	// 修复发送，下一轮应重试同一个 build
	s.failOn = 0
	sent, errs := c.CollectOnce()
	if sent != 1 || errs != 0 || s.count() != 1 {
		t.Errorf("retry round sent=%d errs=%d calls=%d", sent, errs, s.count())
	}
}

// TestCollectOnce_BuildingSkipped 构建中（ok=false）跳过。
func TestCollectOnce_BuildingSkipped(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", model.BuildInfo{}, false, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	sent, errs := c.CollectOnce()
	if sent != 0 || errs != 0 || s.count() != 0 {
		t.Errorf("building build should be skipped: sent=%d errs=%d calls=%d", sent, errs, s.count())
	}
}

// TestCollectOnce_UnknownResultSkipped 未知状态跳过（不发送）。
func TestCollectOnce_UnknownResultSkipped(t *testing.T) {
	info := successInfo("deploy", 42)
	info.Result = "RUNNING"
	b := newMockBuilds()
	b.set("deploy", info, true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	sent, errs := c.CollectOnce()
	if sent != 0 || errs != 0 || s.count() != 0 {
		t.Errorf("unknown result should be skipped: sent=%d errs=%d calls=%d", sent, errs, s.count())
	}
}

// TestCollectOnce_MultipleJobs 多 Job 各自独立采集与去重。
func TestCollectOnce_MultipleJobs(t *testing.T) {
	b := newMockBuilds()
	b.set("job-a", successInfo("job-a", 1), true, nil)
	b.set("job-b", successInfo("job-b", 2), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"job-a", "job-b"}, testLogger())

	sent, errs := c.CollectOnce()
	if sent != 2 || errs != 0 || s.count() != 2 {
		t.Errorf("sent=%d errs=%d calls=%d, want 2/0/2", sent, errs, s.count())
	}
}

// TestCollectOnce_LogFailureDegrades console log 获取失败时降级为空阶段，仍发送。
func TestCollectOnce_LogFailureDegrades(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	logs := &mockLogs{err: errors.New("log unavailable")}
	s := &mockSender{}
	c := newWithDeps(b, logs, s, []string{"deploy"}, testLogger())

	sent, errs := c.CollectOnce()
	if sent != 1 || errs != 0 || s.count() != 1 {
		t.Errorf("log failure should degrade: sent=%d errs=%d calls=%d", sent, errs, s.count())
	}
	if s.sent[0].FailedStage != "" {
		t.Errorf("FailedStage should be empty on log failure: %q", s.sent[0].FailedStage)
	}
}

// TestRun_TickerAndCancel ticker 周期采集，ctx 取消后退出。
func TestRun_TickerAndCancel(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx, 20*time.Millisecond)
		close(done)
	}()

	// Run 立即采一次（去重后只发一次），随后 ticker 轮次不再重复发同一 build。
	time.Sleep(70 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit on cancel")
	}
	if s.count() != 1 {
		t.Errorf("sender calls = %d, want 1 (dedup across ticks)", s.count())
	}
}

// TestRun_NewBuildOnTick ticker 检测到新 build 会发送。
func TestRun_NewBuildOnTick(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx, 20*time.Millisecond); close(done) }()

	time.Sleep(30 * time.Millisecond)
	b.set("deploy", successInfo("deploy", 43), true, nil)
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if s.count() != 2 {
		t.Errorf("sender calls = %d, want 2 (new build on tick)", s.count())
	}
}

// --- 断点续传（Task 5）---

// TestStatePersistence_NoResendAfterRestart 采集成功后断点落盘，
// 新建的 Collector 从断点恢复，不会重复发送同一 build。
func TestStatePersistence_NoResendAfterRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)

	// 第一次运行：发送 build 42，断点写入磁盘。
	s1 := &mockSender{}
	c1 := newWithDeps(b, &mockLogs{}, s1, []string{"deploy"}, testLogger())
	c1.stateFile = statePath
	if sent, _ := c1.CollectOnce(); sent != 1 {
		t.Fatalf("first run sent = %d, want 1", sent)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	// 模拟重启：全新依赖 + 从磁盘恢复断点。
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("Load state: %v", err)
	}
	s2 := &mockSender{}
	c2 := newWithDeps(b, &mockLogs{}, s2, []string{"deploy"}, testLogger())
	c2.stateFile = statePath
	c2.seen = st.Seen // 等价于 NewWithState 的恢复动作
	if sent, _ := c2.CollectOnce(); sent != 0 {
		t.Errorf("after restart should not resend build 42, sent = %d", sent)
	}
	if s2.count() != 0 {
		t.Errorf("after restart sender calls = %d, want 0", s2.count())
	}

	// 来了新 build 43，应正常发送并更新断点。
	b.set("deploy", successInfo("deploy", 43), true, nil)
	if sent, _ := c2.CollectOnce(); sent != 1 {
		t.Errorf("new build should be sent, sent = %d", sent)
	}
	st2, _ := state.Load(statePath)
	if st2.Seen["deploy"] != 43 {
		t.Errorf("state after new build = %+v, want deploy=43", st2.Seen)
	}
}

// TestStatePersistence_SendFailureNotSaved 发送失败时不写断点，重启后仍会重试。
func TestStatePersistence_SendFailureNotSaved(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{failOn: 42}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())
	c.stateFile = statePath

	c.CollectOnce()
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("Load state: %v", err)
	}
	if len(st.Seen) != 0 {
		t.Errorf("failed send must not be persisted, got %+v", st.Seen)
	}
}

// --- 心跳重发（Exporter 重启后恢复 Gauge）---

// newFakeClock 返回可手动推进的时钟，注入 c.now 以精确控制心跳周期。
func newFakeClock() (*time.Time, func() time.Time) {
	t := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	return &t, func() time.Time { return t }
}

// TestResend_DisabledByDefault resendInterval<=0 时不重发（向后兼容）。
func TestResend_DisabledByDefault(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())

	c.CollectOnce()
	c.CollectOnce()
	c.CollectOnce()
	if s.count() != 1 {
		t.Errorf("resend disabled, sender calls = %d, want 1", s.count())
	}
}

// TestResend_ReplaysAfterInterval 到达周期后重发缓存的最新 Build，
// 且重发不更新 seen（断点仍是最初的 build_number）。
func TestResend_ReplaysAfterInterval(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())
	c.stateFile = statePath
	c.SetResendInterval(5 * time.Minute)
	clockPtr, clock := newFakeClock()
	c.now = clock

	c.CollectOnce() // 首次发送，记录计时基准
	if s.count() != 1 {
		t.Fatalf("initial send calls = %d, want 1", s.count())
	}

	*clockPtr = clockPtr.Add(4 * time.Minute)
	c.CollectOnce() // 未到周期，去重跳过
	if s.count() != 1 {
		t.Errorf("before interval calls = %d, want 1", s.count())
	}

	*clockPtr = clockPtr.Add(2 * time.Minute) // 累计 6 分钟，超过 5 分钟周期
	c.CollectOnce()
	if s.count() != 2 {
		t.Errorf("after interval calls = %d, want 2 (heartbeat resend)", s.count())
	}
	if s.sent[1].BuildNumber != 42 {
		t.Errorf("resent build_number = %d, want 42", s.sent[1].BuildNumber)
	}

	// 重发不改断点：仍是 42，且下一轮仍按周期计时（不会每轮都发）。
	st, _ := state.Load(statePath)
	if st.Seen["deploy"] != 42 {
		t.Errorf("state after resend = %+v, want deploy=42", st.Seen)
	}
	c.CollectOnce()
	if s.count() != 2 {
		t.Errorf("immediately after resend calls = %d, want 2 (timer reset)", s.count())
	}
}

// TestResend_NewBuildResetsTimer 新 build 正常发送后重置心跳计时。
func TestResend_NewBuildResetsTimer(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())
	c.SetResendInterval(5 * time.Minute)
	clockPtr, clock := newFakeClock()
	c.now = clock

	c.CollectOnce() // 42 发送
	*clockPtr = clockPtr.Add(4 * time.Minute)
	b.set("deploy", successInfo("deploy", 43), true, nil)
	c.CollectOnce() // 43 是新 build，正常发送并重置计时
	if s.count() != 2 {
		t.Fatalf("two builds calls = %d, want 2", s.count())
	}

	*clockPtr = clockPtr.Add(4 * time.Minute) // 距 43 仅 4 分钟，不应重发
	c.CollectOnce()
	if s.count() != 2 {
		t.Errorf("timer not reset after new build, calls = %d, want 2", s.count())
	}
	*clockPtr = clockPtr.Add(2 * time.Minute) // 距 43 共 6 分钟
	c.CollectOnce()
	if s.count() != 3 {
		t.Errorf("heartbeat after new build calls = %d, want 3", s.count())
	}
	if s.sent[2].BuildNumber != 43 {
		t.Errorf("resent build_number = %d, want 43 (latest)", s.sent[2].BuildNumber)
	}
}

// TestResend_FailedResendDoesNotAdvanceTimer 心跳发送失败时不推进计时，下轮立即重试。
func TestResend_FailedResendDoesNotAdvanceTimer(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())
	c.SetResendInterval(5 * time.Minute)
	clockPtr, clock := newFakeClock()
	c.now = clock

	c.CollectOnce()
	*clockPtr = clockPtr.Add(6 * time.Minute)

	// 让重发失败：failOn 只在重发时刻设置，第一次正常发送时 s.failOn=0。
	s.failOn = 42
	sent, errs := c.CollectOnce()
	if sent != 0 || errs != 1 || s.count() != 1 {
		t.Errorf("failed resend: sent=%d errs=%d calls=%d, want 0/1/1", sent, errs, s.count())
	}
	// 计时未推进，修复后下一轮立刻重试。
	s.failOn = 0
	c.CollectOnce()
	if s.count() != 2 {
		t.Errorf("retry after failed resend calls = %d, want 2", s.count())
	}
}

// TestResend_MultipleJobs 每个 job 独立计时，已发送的全部按周期重发。
func TestResend_MultipleJobs(t *testing.T) {
	b := newMockBuilds()
	b.set("job-a", successInfo("job-a", 1), true, nil)
	b.set("job-b", successInfo("job-b", 2), true, nil)
	s := &mockSender{}
	c := newWithDeps(b, &mockLogs{}, s, []string{"job-a", "job-b"}, testLogger())
	c.SetResendInterval(5 * time.Minute)
	clockPtr, clock := newFakeClock()
	c.now = clock

	c.CollectOnce()
	if s.count() != 2 {
		t.Fatalf("initial calls = %d, want 2", s.count())
	}
	*clockPtr = clockPtr.Add(6 * time.Minute)
	sent, errs := c.CollectOnce()
	if sent != 2 || errs != 0 || s.count() != 4 {
		t.Errorf("heartbeat: sent=%d errs=%d calls=%d, want 2/0/4", sent, errs, s.count())
	}
}

// TestResend_NoResendBeforeFirstSend 从未成功发送过的 job 不会凭空重发。
func TestResend_NoResendBeforeFirstSend(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{failOn: 42} // 首次发送失败，无缓存
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())
	c.SetResendInterval(5 * time.Minute)
	clockPtr, clock := newFakeClock()
	c.now = clock

	c.CollectOnce() // 发送失败，不缓存
	*clockPtr = clockPtr.Add(10 * time.Minute)
	s.failOn = 0
	c.CollectOnce() // shouldProcess 成立（seen 未更新）→ 正常发送，不是心跳
	if s.count() != 1 {
		t.Errorf("calls = %d, want 1 (normal send, not heartbeat)", s.count())
	}
}

// TestResend_AfterRestartRestoresGauge 模拟 Exporter 与 Collector 同时重启：
// Collector 从断点恢复 seen，但本进程无缓存、无计时基准。首轮必须立刻心跳重发
// （且从 Jenkins 现场重建 Build，不依赖内存缓存），这样 Exporter 内存清空后
// Gauge 才能恢复——这正是现场"一半有数据一半 No data"的根因场景。
func TestResend_AfterRestartRestoresGauge(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)

	// 第一次运行：发送成功，断点落盘。
	c1 := newWithDeps(b, &mockLogs{}, &mockSender{}, []string{"deploy"}, testLogger())
	c1.stateFile = statePath
	c1.SetResendInterval(5 * time.Minute)
	if sent, _ := c1.CollectOnce(); sent != 1 {
		t.Fatalf("first run sent = %d, want 1", sent)
	}

	// 模拟双重启：依赖全换、seen 从 state 恢复，但 latestBuild/lastResendAt 为空。
	st, _ := state.Load(statePath)
	s2 := &mockSender{}
	c2 := newWithDeps(b, &mockLogs{}, s2, []string{"deploy"}, testLogger())
	c2.stateFile = statePath
	c2.seen = st.Seen // 等价于 NewWithState 恢复
	c2.SetResendInterval(5 * time.Minute)

	sent, errs := c2.CollectOnce()
	if sent != 1 || errs != 0 {
		t.Fatalf("after double restart sent=%d errs=%d, want 1/0", sent, errs)
	}
	if s2.count() != 1 || s2.sent[0].BuildNumber != 42 {
		t.Errorf("heartbeat did not restore build: calls=%d builds=%+v", s2.count(), s2.sent)
	}
	// 重发不改断点，也不因缓存缺失而漏发未知状态。
	if got := s2.sent[0].Status; got != model.StatusSuccess {
		t.Errorf("resent status = %q, want success", got)
	}
}

// TestResend_NoResendForBrandNewJob 断点无记录的全新 job 不会因心跳配置而空发。
func TestResend_NoResendForBrandNewJob(t *testing.T) {
	b := newMockBuilds()
	b.set("deploy", successInfo("deploy", 42), true, nil)
	s := &mockSender{failOn: 42} // 首次失败 → seen 无记录
	c := newWithDeps(b, &mockLogs{}, s, []string{"deploy"}, testLogger())
	c.SetResendInterval(time.Nanosecond) // 极短周期，若误判会立刻补发
	c.CollectOnce()
	if s.count() != 0 {
		t.Errorf("brand-new unsent job should not heartbeat, calls = %d", s.count())
	}
}
