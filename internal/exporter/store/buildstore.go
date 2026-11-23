package store

// 本文件实现动态化改造 Task 2 的内存数据存储：BuildStore。
//
// 为什么需要它：文件来源每轮全量重读、天然无状态；但 HTTP 推送是事件式的
// （推一次要一直持有），必须在内存里保存每个 Job 的最新 Build。
// 这是现有架构里唯一真正缺失的部件（见 docs/task1-architecture.md）。
//
// 存储策略：
//   - 只存每个 job 的**最新** Build，不保留历史（Task 5 明确"不要无限保留 Build 历史"；
//     趋势由 Prometheus 自己的时序承担，这正是 Prometheus 的本职）。
//   - build_number 作为新旧判定依据：Jenkins 同一 Job 单调递增，
//     不大于已存值视为旧数据/重复提交，拒绝，保证幂等。
//   - 所有方法用同一把 Mutex 保护，因为 ingest handler 可能被多个 goroutine 并发调用。

import (
	"sync"
	"time"

	"ops-exporter/internal/exporter/model"
)

// BuildStore 在内存中保存每个 Jenkins Job 的最新一次 Build。
// 零值可用（无需 New 函数），便于测试与 DI。
type BuildStore struct {
	mu     sync.Mutex
	latest map[string]model.BuildResult // key: job_name
}

// Save 保存一次 Build，返回是否被接受。
//
// 接受条件：该 job 尚无记录，或新 Build 的 build_number 严格大于已存值。
// 拒绝场景（返回 false）：
//   - 重复提交：build_number 与已存值相同（幂等，Collector 重试不会产生副作用）；
//   - 旧数据乱序到达：build_number 小于已存值（集合点/重放不会覆盖新状态）。
//
// 注意：被拒绝时既不改存储也不写指标，调用方据此决定是否更新指标，
// 否则重复提交会让 Counter 重复累加。
func (s *BuildStore) Save(b model.BuildResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.latest == nil {
		s.latest = make(map[string]model.BuildResult)
	}
	if old, ok := s.latest[b.JobName]; ok && b.BuildNumber <= old.BuildNumber {
		return false
	}
	s.latest[b.JobName] = b
	return true
}

// Snapshot 返回当前所有 job 最新 Build 的副本。
// 返回副本而非内部引用：调用方改动结果不会影响存储，避免数据竞争。
// 顺序不确定（map 遍历），测试需用 key 比对而非下标。
func (s *BuildStore) Snapshot() []model.BuildResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]model.BuildResult, 0, len(s.latest))
	for _, b := range s.latest {
		out = append(out, b)
	}
	return out
}

// Get 返回指定 job 的最新 Build。
// ok=false 表示该 job 从未提交过。返回值是副本。
func (s *BuildStore) Get(jobName string) (model.BuildResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.latest[jobName]
	return b, ok
}

// Prune 清理超过 maxAge 未更新的 Build，返回被清理的 job 名（供指标层删除对应序列）。
//
// 判定依据：now.Unix()-b.Timestamp > maxAge 秒。用最新 Build 的结束时刻而非
// "上次收到推送的时刻"——Collector 停摆或 Job 被删除时，即便没有新推送进来，
// 旧数据也会自然过期，避免僵尸 Job 的 Gauge 永久停留在旧值（Task 5 数据过期）。
//
// maxAge<=0 表示禁用清理（返回空切片），保持向后兼容。
// now 由调用方传入而非 time.Now()：清理阈值是运维关心的行为，注入时间便于精确测试边界。
func (s *BuildStore) Prune(maxAge time.Duration, now time.Time) []string {
	if maxAge <= 0 {
		return nil
	}
	cutoff := now.Add(-maxAge).Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []string
	for job, b := range s.latest {
		if b.Timestamp < cutoff {
			delete(s.latest, job)
			removed = append(removed, job)
		}
	}
	return removed
}
