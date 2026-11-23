package store

import (
	"sync"
	"testing"
	"time"

	"ops-exporter/internal/exporter/model"
)

func testBuild(job string, number int64, status string) model.BuildResult {
	return model.BuildResult{
		JobName:     job,
		BuildNumber: number,
		Status:      status,
		Duration:    10,
		Timestamp:   sampleTimestamp + number,
	}
}

// TestBuildStore_SaveAndGet 正常保存与读取：首次保存应被接受且能取回。
func TestBuildStore_SaveAndGet(t *testing.T) {
	var s BuildStore
	b := testBuild("pipeline", 1, model.BuildStatusSuccess)

	if !s.Save(b) {
		t.Fatal("first save should be accepted")
	}
	got, ok := s.Get("pipeline")
	if !ok {
		t.Fatal("Get should find the saved job")
	}
	if got.BuildNumber != 1 || got.Status != model.BuildStatusSuccess {
		t.Errorf("Get = %+v, want build_number=1 status=success", got)
	}
}

// TestBuildStore_NewBuildWins 新旧规则：更大的 build_number 覆盖，更小的被拒绝。
func TestBuildStore_NewBuildWins(t *testing.T) {
	var s BuildStore
	s.Save(testBuild("pipeline", 5, model.BuildStatusFailed))

	// 更大的编号：接受并覆盖
	if !s.Save(testBuild("pipeline", 6, model.BuildStatusSuccess)) {
		t.Fatal("newer build_number should be accepted")
	}
	got, _ := s.Get("pipeline")
	if got.BuildNumber != 6 || got.Status != model.BuildStatusSuccess {
		t.Errorf("latest = %+v, want build_number=6 status=success", got)
	}

	// 更小的编号（乱序/重放）：拒绝，不能覆盖新状态
	if s.Save(testBuild("pipeline", 4, model.BuildStatusFailed)) {
		t.Fatal("older build_number should be rejected")
	}
	got, _ = s.Get("pipeline")
	if got.BuildNumber != 6 {
		t.Errorf("stale build overwrote latest: build_number=%d", got.BuildNumber)
	}
}

// TestBuildStore_DuplicateIsIdempotent 重复提交：相同 build_number 必须被拒绝（幂等），
// 这样 Collector 重试不会发生副作用，也不会让 Counter 重复累加。
func TestBuildStore_DuplicateIsIdempotent(t *testing.T) {
	var s BuildStore
	first := testBuild("pipeline", 3, model.BuildStatusFailed)
	if !s.Save(first) {
		t.Fatal("first save should be accepted")
	}

	// 完全相同编号，即便其他字段不同，也应拒绝
	dup := testBuild("pipeline", 3, model.BuildStatusSuccess)
	dup.Duration = 999
	if s.Save(dup) {
		t.Fatal("duplicate build_number should be rejected")
	}
	got, _ := s.Get("pipeline")
	if got.Status != model.BuildStatusFailed || got.Duration != 10 {
		t.Errorf("duplicate overwrote original: %+v", got)
	}
}

// TestBuildStore_MultipleJobs 不同 job 互不干扰，各自保存自己的最新 Build。
func TestBuildStore_MultipleJobs(t *testing.T) {
	var s BuildStore
	s.Save(testBuild("job-a", 1, model.BuildStatusSuccess))
	s.Save(testBuild("job-b", 2, model.BuildStatusFailed))
	s.Save(testBuild("job-a", 3, model.BuildStatusUnstable))

	a, okA := s.Get("job-a")
	b, okB := s.Get("job-b")
	if !okA || !okB {
		t.Fatalf("both jobs should exist: okA=%v okB=%v", okA, okB)
	}
	if a.BuildNumber != 3 {
		t.Errorf("job-a build_number = %d, want 3", a.BuildNumber)
	}
	if b.BuildNumber != 2 {
		t.Errorf("job-b build_number = %d, want 2", b.BuildNumber)
	}
	if got := len(s.Snapshot()); got != 2 {
		t.Errorf("Snapshot len = %d, want 2", got)
	}
}

// TestBuildStore_GetMissing 未提交过的 job 返回 ok=false。
func TestBuildStore_GetMissing(t *testing.T) {
	var s BuildStore
	if _, ok := s.Get("never-seen"); ok {
		t.Fatal("Get on empty store should return ok=false")
	}
}

// TestBuildStore_SnapshotIsCopy Snapshot 返回副本：改动返回值不影响内部存储。
func TestBuildStore_SnapshotIsCopy(t *testing.T) {
	var s BuildStore
	s.Save(testBuild("pipeline", 1, model.BuildStatusSuccess))

	snap := s.Snapshot()
	snap[0].Status = model.BuildStatusFailed
	snap[0].BuildNumber = 999

	got, _ := s.Get("pipeline")
	if got.Status != model.BuildStatusSuccess || got.BuildNumber != 1 {
		t.Errorf("mutating snapshot affected store: %+v", got)
	}
}

// TestBuildStore_ConcurrentSaves 并发安全：多 goroutine 同时提交不同编号，
// 最终必须留下最大编号，且不能 panic（配合 -race / 重复运行验证无数据竞争）。
func TestBuildStore_ConcurrentSaves(t *testing.T) {
	var s BuildStore
	const n = 100

	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(num int64) {
			defer wg.Done()
			s.Save(testBuild("pipeline", num, model.BuildStatusSuccess))
		}(int64(i))
	}
	wg.Wait()

	got, ok := s.Get("pipeline")
	if !ok {
		t.Fatal("job should exist after concurrent saves")
	}
	if got.BuildNumber != n {
		t.Errorf("final build_number = %d, want %d", got.BuildNumber, n)
	}
}

// TestBuildStore_Prune 过期清理：只删除超过 maxAge 的 job，返回被清理的名字。
func TestBuildStore_Prune(t *testing.T) {
	var s BuildStore
	// sampleTimestamp 作为"当前时间"基准；构造 3 个不同新鲜度的 job。
	fresh := testBuild("fresh", 1, model.BuildStatusSuccess)
	fresh.Timestamp = sampleTimestamp
	stale := testBuild("stale", 1, model.BuildStatusFailed)
	stale.Timestamp = sampleTimestamp - 3600 // 1 小时前
	s.Save(fresh)
	s.Save(stale)

	now := time.Unix(sampleTimestamp, 0)
	removed := s.Prune(30*time.Minute, now)
	if len(removed) != 1 || removed[0] != "stale" {
		t.Errorf("Prune removed = %v, want [stale]", removed)
	}
	if _, ok := s.Get("stale"); ok {
		t.Error("stale job should be removed")
	}
	if _, ok := s.Get("fresh"); !ok {
		t.Error("fresh job should remain")
	}
}

// TestBuildStore_PruneBoundary 边界：恰好等于 maxAge 不算过期（严格大于才清理）。
func TestBuildStore_PruneBoundary(t *testing.T) {
	var s BuildStore
	b := testBuild("boundary", 1, model.BuildStatusSuccess)
	b.Timestamp = sampleTimestamp
	s.Save(b)

	now := time.Unix(sampleTimestamp+3600, 0)
	if got := s.Prune(1*time.Hour, now); len(got) != 0 {
		t.Errorf("exactly-at-maxAge should not be pruned, got %v", got)
	}
	if got := s.Prune(3599*time.Second, now); len(got) != 1 {
		t.Errorf("just-over-maxAge should be pruned, got %v", got)
	}
}

// TestBuildStore_PruneDisabled maxAge<=0 禁用清理：任何数据都不删。
func TestBuildStore_PruneDisabled(t *testing.T) {
	var s BuildStore
	b := testBuild("old", 1, model.BuildStatusSuccess)
	b.Timestamp = sampleTimestamp
	s.Save(b)

	if got := s.Prune(0, time.Unix(sampleTimestamp+999999, 0)); len(got) != 0 {
		t.Errorf("disabled prune should remove nothing, got %v", got)
	}
	if _, ok := s.Get("old"); !ok {
		t.Error("job should remain when pruning disabled")
	}
}
