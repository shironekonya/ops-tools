# Task 1：现有工程梳理与增量改造设计（分析报告）

> 只分析，未修改任何代码。基线：`go vet` 干净，`go test ./...` 全绿（61 个用例）。
> 本文档中的"现有工程"即当前仓库（module 名 `ops-exporter`，任务文档中称 test-exporter，同一项目）。
>
> **结构演进提示（合并单 module 后追加）**：本文写于改造启动前，"目标文件布局"一节按当时约定记录为"平铺 package main + jenkins-collector 独立 module"。后续工程化已作废"不拆 package"约定；当前最终形态为**单一 module `ops-exporter` + 双入口**：
> `cmd/exporter`、`cmd/collector`，实现分置 `internal/exporter/{config,model,reader,refresher,metrics,store,ingest,expiry}` 与 `internal/collector/{config,model,jenkins,parser,sender,collector,state}` 两个互不相交的子树；无 `go.work`、无子 `go.mod`。本文的**设计原则与数据流分析依然有效**，仅文件布局以 `HANDOFF.md` 的"当前工程结构"为准。

---

## 一、现有工程架构

### 各文件职责（单文件单一职责，全在 package main）

| 文件 | 职责 | 关键导出 |
|---|---|---|
| `main.go` | 启动编排：加载配置 → 建日志 → 建 registry/指标 → 启动刷新 goroutine → 启动 HTTP → 优雅退出 | `main`, `handleHealthz` |
| `config.go` | JSON 配置加载与校验（监听地址/结果目录/刷新间隔/日志级别），文件缺失走默认值，非法即退出 | `Config`, `LoadConfig` |
| `task.go` | **统一数据模型**：`TaskResult` 结构 + `ParseTaskResult`（解析+校验）+ `Duration()` | `TaskResult`, `ParseTaskResult`, `StatusSuccess/Failed` |
| `reader.go` | **文件来源适配器**：扫描目录 → 逐文件解析 → 三段式返回（结果/文件级错误/目录级错误） | `LoadTaskResults`, `FileError` |
| `refresher.go` | **周期驱动器**：ticker 循环调 reader→updater；`refreshTracker` 做日志降噪 | `RunRefresher`, `refreshOnce`, `refreshTracker` |
| `updater.go` | **模型→指标转换器**：单条 `Update` + 批量 `ApplyResults`（按 end_time 去重） | `JobMetrics.Update`, `JobMetrics.ApplyResults` |
| `metrics.go` | **指标容器**：5 个指标定义与注册（4 个 GaugeVec + 1 个 Gauge），注入 registry | `JobMetrics`, `NewJobMetrics` |

### 当前数据流（单一来源：JSON 文件）

```
磁盘 JSON 文件（结果目录，每文件一个对象）
   │ os.ReadDir + os.ReadFile          ← reader.go
   ▼
ParseTaskResult（语法校验 + 业务校验）  ← task.go
   │ results[] / fileErrs[] / dirErr
   ▼
refreshOnce（每 10s 一轮）             ← refresher.go
   ├─ fileErrs → refreshTracker.reportFiles（首次 Warn/重复 Debug/恢复静默）
   ├─ dirErr  → refreshTracker.reportDir（首次 Error/重复 Debug/恢复 Info）
   └─ results → ApplyResults            ← updater.go
                 ├─ 按 job_name 去重（end_time 最新者胜）
                 └─ Update: 写 4 个 GaugeVec 对应序列
                      ▼
              /metrics（registry 内存快照，Prometheus 每 15s 来抓）
```

### 并发模型（改造的关键前提）

- **只有两个 goroutine**：主 goroutine（HTTP + 信号等待）和刷新 goroutine。
- `GaugeVec.Set/WithLabelValues` 内部自带锁、原子——**updater.go 注释已明确允许多 goroutine 并发调用**。这是 Task 2 直接复用 `Update` 的合法性依据。
- `refreshTracker` 只被刷新 goroutine 持有，无锁。
- 退出顺序：cancel 刷新 ctx → wg.Wait() → srv.Shutdown(5s)。

### 健康检查

`/healthz` 固定返回 `{"status":"ok"}`（JSON），写失败明确忽略+注释。**保留原样**。

---

## 二、调用关系问答

| 问题 | 答案 |
|---|---|
| 谁负责读取 JSON？ | `reader.go`（从文件），`task.go` 的 `ParseTaskResult`（从字节） |
| 谁负责刷新数据？ | `refresher.go` 的 `RunRefresher/refreshOnce`（周期驱动，错误降噪） |
| 谁负责更新数据？ | `updater.go` 的 `Update/ApplyResults`（写入 GaugeVec） |
| 谁负责生成指标？ | `metrics.go` 定义并注册；Prometheus 抓取时由 registry 实时 Gather，无缓存层 |
| 数据如何传递？ | 磁盘 → `[]byte` → `TaskResult` → `ApplyResults` → GaugeVec → registry → `/metrics` 文本。**全内存单 pass，无中间存储**——文件来源是"每轮重读"，所以不需要状态存储 |

**核心洞察（决定改造方案）**：现有架构"无状态"是因为文件来源每轮全量重读。HTTP 推送来源是**事件式**的（推一次、要一直持有），所以 Task 2 必须引入一个**内存状态存储**，这是现有代码里唯一真正缺失的部件。

---

## 三、目标结构设计

### 设计原则

1. **两种来源、两条管道、共享基础设施**。文件管道不动一行逻辑；HTTP 管道是新增的平行管道。二者共用：config / logger / registry / mux / 优雅退出。
2. Jenkins 数据走**独立的 `jenkins_*` 指标命名空间**（Task 4 明确要求 `jenkins_build_*`），不与 `ops_job_*` 混用——两者语义不同（job 无 build_number 维度，build 有）。
3. 不拆 package、不引入框架；新功能按现有惯例一文件一职责。

### 目标文件布局（增量，全部在 package main）

```
main.go            # 小改：多挂一条路由、多建一个 store/BuildMetrics
config.go          # 小改：新增可选配置段（ingest 相关），向后兼容（缺省=关闭或默认开启？见实施计划）
task.go            # 不动
reader.go          # 不动
refresher.go       # 不动
updater.go         # 不动（文件管道专用）
metrics.go         # 不动（ops_job_* 专用）
+ build.go         # 新增：BuildResult 数据模型 + ParseBuildResult 校验（对齐 task.go 的模式）
+ buildstore.go    # 新增：内存存储（map[job]最新 Build，sync.Mutex），去重/新旧规则在这里
+ buildmetrics.go  # 新增：jenkins_build_* 指标定义与注册（对齐 metrics.go 的模式）
+ buildupdater.go  # 新增：BuildResult → jenkins_* 指标转换（对齐 updater.go 的模式）
+ ingest.go        # 新增：POST /api/v1/ingest/build handler（限流、超时、错误响应规范）
+ ingest_test.go / build_test.go / buildstore_test.go / buildupdater_test.go
jenkins-collector/ # Task 3 新增时的原计划（独立 module）；最终已合并进 cmd/collector + internal/collector/*，见文首结构演进提示
```

### 各问题的落点

**1. HTTP 接收入口放在哪里？**
`main.go` 的现有 `mux` 上挂 `/api/v1/ingest/build`，handler 实现在新文件 `ingest.go`。与 `/healthz`、`/metrics` 同一 Server、同一 registry。理由：一个进程一个端口是现有部署模型，拆端口违反"不破坏现有部署"。

**2. 数据校验放在哪里？**
`build.go` 的 `ParseBuildResult`，模式完全对齐 `task.go`：`json.Unmarshal`（语法/类型错误在此暴露）→ `validate()`（必填、状态值、范围检查）。**不复用 TaskResult.validate**——字段集不同（build_number/duration/timestamp/failed_stage），强行复用会造出一个"什么都装"的上帝模型，违反文档"不要设计过于通用的动态 JSON 框架"。

**3. 内存数据存储放在哪里？**
新文件 `buildstore.go`：

```go
type BuildStore struct {
    mu     sync.Mutex
    latest map[string]BuildResult // key: job_name
}
// Save(b BuildResult) (accepted bool)  —— build_number 不大于已存值则拒绝（新旧规则）
// Snapshot() []BuildResult             —— 供测试与未来快照使用
```

只存每个 job 的**最新 Build**（Task 5 明确"不要无限保留 Build 历史"，历史/趋势由 Prometheus 自己的时序承担，这正是 Prometheus 的设计本职）。文件来源不需要它，它只服务推送来源。

**4. 原有 JSON Reader 如何复用？**
**原样复用，零改动**。`RunRefresher → LoadTaskResults → ApplyResults` 全链路保持。文件来源与推送来源互不覆盖：文件写 `ops_job_*`，推送写 `jenkins_build_*`。

**5. Jenkins 数据如何进入现有更新流程？**
两条路，分属两个 Task：

```
Jenkins Master
   │  (Task 3: jenkins-collector 独立程序，REST API + token 认证)
   │  采集 → 标准化为 BuildResult JSON
   ▼  HTTP POST /api/v1/ingest/build
ingest.go（Task 2）
   ├─ 限流/超时 → ParseBuildResult（build.go 校验）
   ├─ BuildStore.Save（去重：同 job 旧 build_number 拒绝，重复提交幂等）
   └─ BuildMetrics.Update（buildupdater.go，立即写指标，不等刷新周期）
         ▼
   jenkins_build_status / duration / last_timestamp (GaugeVec)
   jenkins_build_total / failure_total (CounterVec)
         ▼
   /metrics（与 ops_job_* 同一 registry，一次抓取全拿到）
```

设计取舍：ingest handler **收到即写指标**（不经过 ticker）。依据：GaugeVec 并发安全已由 updater.go 注释背书；推送本来就是事件驱动，等周期反而引入无意义的延迟。`refresher` 的"周期 + 降噪"模式不适合事件源，不强行套用（文档：不要强行让不适合的模块承担新职责）。

**6. Counter 的正确性（Task 4 前置约束）**
`jenkins_build_total` 等 Counter **不能存内存重启归零后乱跳**——v1 就用它，但注明：重启后 Counter 从 0 重新计数，Prometheus 的 `rate()`/`increase()` 对重启有内建容忍（计数器重置处理），无需自己做持久化。这一点写进指标 Help 文本。

### 配置兼容性

`config.json` 现有四字段原样保留、语义不变。新增字段采用**可选段**（缺省走默认值），不破坏现有配置文件与测试：

```json
{
  "listen_addr": ":9105",
  "result_dir": "data",
  "refresh_interval": "10s",
  "log_level": "info",
  "ingest": { "max_body_bytes": 1048576 }
}
```

---

## 四、后续任务实施计划

| 任务 | 改动范围 | 依赖 | 关键验收 |
|---|---|---|---|
| Task 2 | `+build.go +buildstore.go +buildmetrics.go +buildupdater.go +ingest.go +各测试`，`main.go/config.go` 小改 | 无 | curl POST 正常/错误/缺字段/重复/并发 5 类场景；原有 61 用例不破坏 |
| Task 3 | 原计划 `+jenkins-collector/`（独立 go.mod）；最终落地为 `cmd/collector` + `internal/collector/*`（同 module，独立入口）：api client / 日志解析 / 发送器 / ticker | Task 2 的接口契约 | 真实 Jenkins 构建后 Collector 能拿到并发给 Exporter |
| Task 4 | Dashboard 新增 jenkins_* 面板；Prometheus scrape 验证 | Task 2+3 | Jenkins 构建 → Grafana 展示全链路闭环 |
| Task 5 | 请求限制/超时/优雅退出完善、Collector 断点与重试、数据过期、Dockerfile/Compose、部署文档 | Task 1~4 | 可独立部署的完整监控系统 |

建议的 Task 2 内部顺序：`build.go`（模型+校验）→ `buildstore.go`（存储+去重）→ `buildmetrics.go`+`buildupdater.go`（转换）→ `ingest.go`（HTTP 编排）→ `main.go` 接线 → 每步 `go test ./...`。

---

## 五、按任务格式输出

1. **修改了哪些文件**：无（仅新增本文档）。
2. **为什么修改**：Task 1 明确只分析不改码。
3. **核心逻辑说明**：见上文架构/调用关系/目标结构三节。一句话版本——现有工程是"文件来源、无状态、单 goroutine 写指标"的管道；动态化只需要在旁边加一条"推送来源、有状态（BuildStore）、事件驱动写指标"的平行管道，共享 config/logger/registry/HTTP 基础设施，原有文件管道零改动。
4. **测试结果**：`go vet ./...` 干净；`go test ./...` 全绿（61 用例，2.0s）——基线健康，可以开始 Task 2。
5. **已知问题**：
   - Linux 虚拟机上当前运行的是源码的一个变体（healthz 返回纯文本、指标名 `ops_job_last_refresh_*` 与仓库不一致），Task 2 部署验证前需以仓库版本为准统一；
   - 现有 reader 只支持"一文件一 JSON 对象"，不识别多对象拼接/NDJSON（Linux 实测踩过），Task 2 的 HTTP 接口天然规避此问题，但文件管道语义不变；
   - `broken_task` 等演示数据仍留在 data/ 目录，接入真实 Jenkins 数据前应清理。
6. **下一任务前置条件**：本文档方案获确认后即可开始 Task 2；无需其他准备（依赖已就绪：prometheus client_golang、标准库 net/http、61 个回归测试作为保护网）。
