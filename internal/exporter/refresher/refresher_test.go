package refresher

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// writeTaskFile 写一个任务结果 JSON 文件到目录。
func writeTaskFile(t *testing.T, dir, file, job, status string, end time.Time) {
	t.Helper()
	content := `{
		"job_name": "` + job + `",
		"status": "` + status + `",
		"start_time": "` + end.Add(-time.Minute).Format(time.RFC3339) + `",
		"end_time": "` + end.Format(time.RFC3339) + `",
		"exit_code": 0
	}`
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

// gaugeStatus 读 last_status 当前值，序列不存在返回 (0, false)。
func gaugeStatus(t *testing.T, reg *prometheus.Registry, job string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "ops_job_last_status" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == jobLabel && lp.GetValue() == job {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// waitForStatus 轮询等待某任务的 last_status 变成期望值。
// 超过 deadline 仍不满足则测试失败。轮询而不是 sleep 固定时长，
// 既不拖慢测试也不依赖刷新间隔的精确时序。
func waitForStatus(t *testing.T, reg *prometheus.Registry, job string, want float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := gaugeStatus(t, reg, job); ok && v == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, ok := gaugeStatus(t, reg, job)
	t.Fatalf("job %q status = %v (exist=%v) after %v, want %v", job, v, ok, timeout, want)
}

// TestRefreshOnce 验证单轮刷新：合法文件进指标，坏文件被跳过不崩溃。
func TestRefreshOnce(t *testing.T) {
	m, reg := newTestMetrics(t)
	dir := t.TempDir()

	end := time.Now()
	writeTaskFile(t, dir, "good.json", "database_backup", "success", end)
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatalf("write bad.json: %v", err)
	}

	if err := refreshOnce(dir, m, newRefreshTracker()); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	if v, ok := gaugeStatus(t, reg, "database_backup"); !ok || v != 1 {
		t.Errorf("database_backup status = %v (exist=%v), want 1", v, ok)
	}
}

// TestRefreshOnce_DirMissing 验证目录级失败返回 error 而不是 panic。
func TestRefreshOnce_DirMissing(t *testing.T) {
	m, _ := newTestMetrics(t)
	if err := refreshOnce(filepath.Join(t.TempDir(), "nope"), m, newRefreshTracker()); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

// TestRunRefresher_FileChange 核心验收：文件更新后，指标随刷新周期更新。
func TestRunRefresher_FileChange(t *testing.T) {
	m, reg := newTestMetrics(t)
	dir := t.TempDir()

	end := time.Now()
	writeTaskFile(t, dir, "backup.json", "database_backup", "success", end)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunRefresher(ctx, dir, 30*time.Millisecond, m)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("refresher did not stop in time")
		}
	}()

	// 初始文件是成功状态
	waitForStatus(t, reg, "database_backup", 1, 2*time.Second)

	// 覆盖写入失败状态（end_time 更新，模拟任务新一轮执行）
	time.Sleep(50 * time.Millisecond) // 确保新的 end_time 更晚
	writeTaskFile(t, dir, "backup.json", "database_backup", "failed", end.Add(30*time.Second))
	waitForStatus(t, reg, "database_backup", 0, 2*time.Second)
}

// TestRunRefresher_ConcurrentScrape 验收：刷新进行中反复并发抓取 /metrics
// 不发生数据竞争（本测试需配合 go test -race 才有检查效果）。
func TestRunRefresher_ConcurrentScrape(t *testing.T) {
	// 静音日志：writer 会持续写文件，refresher 必然频繁读到半截 JSON，
	// 这些 Warn 是本测试的预期现象，输出出来只会刷屏。
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	})

	m, reg := newTestMetrics(t)
	dir := t.TempDir()

	end := time.Now()
	writeTaskFile(t, dir, "sync.json", "file_sync", "success", end)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunRefresher(ctx, dir, 5*time.Millisecond, m)

	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()

	// 后台持续改写文件，模拟任务方不停写结果。
	// stopWriting 只由主 goroutine 关闭，writer 只读它，避免双向等待死锁。
	stopWriting := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		i := 0
		for {
			select {
			case <-stopWriting:
				return
			default:
				i++
				writeTaskFile(t, dir, "sync.json", "file_sync",
					map[bool]string{true: "success", false: "failed"}[i%2 == 0],
					end.Add(time.Duration(i)*time.Second))
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// 并发抓取 200 次
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Errorf("scrape: %v", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("scrape status = %d", resp.StatusCode)
			}
			_, _ = io.ReadAll(resp.Body)
		}()
	}
	wg.Wait()
	close(stopWriting) // 通知 writer 停止
	<-writerDone       // 等它真正退出
}

// TestRunRefresher_StopsOnCancel 验收：ctx 取消后刷新循环退出。
func TestRunRefresher_StopsOnCancel(t *testing.T) {
	m, _ := newTestMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		RunRefresher(ctx, t.TempDir(), time.Hour, m) // 间隔拉长，确保阻塞在 select 上
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// 正常退出
	case <-time.After(2 * time.Second):
		t.Fatal("refresher did not stop after cancel")
	}
}

// TestRunRefresher_DirRecovery 刷新异常的恢复路径：
// 目录一开始不存在（每轮 Error），中途创建出来后，
// 不重启进程、指标就能在后续周期正常出现。
// 这验证了"目录级错误不终止循环"的承诺真实成立。
func TestRunRefresher_DirRecovery(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	})

	m, reg := newTestMetrics(t)
	outer := t.TempDir()
	dir := filepath.Join(outer, "results") // 故意不创建

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunRefresher(ctx, dir, 20*time.Millisecond, m)

	// 等一小段时间让前几轮失败（日志已静音，无需断言内容）
	time.Sleep(80 * time.Millisecond)

	// 目录出现（模拟运维把结果目录补上）
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTaskFile(t, dir, "backup.json", "database_backup", "success", time.Now())

	// 轮询等待指标出现：不需要重启 exporter
	waitForStatus(t, reg, "database_backup", 1, 2*time.Second)
}
