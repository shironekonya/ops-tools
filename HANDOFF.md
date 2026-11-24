# 交接提示词（粘贴到新对话即可继续）

你是我的开发助手，继续一个进行中的 Go 项目。请先按顺序读取必读文件恢复上下文，然后执行当前任务。

## 必读文件（按顺序）

1. `C:\Users\shironeko\WorkBuddy\2026-11-18-16-36-15\.workbuddy\memory\2026-11-18.md` —— 项目全部历史（TODO 1~12、Task 2、Task 3、工程化重构、合并单 module 的完整记录与踩坑）
2. `docs\task1-architecture.md` —— 动态化改造 Task 1 的架构分析与 Task 2~5 设计定稿。**注意：该文档的"目标文件布局"一节仍保留拆包前的平铺结构原文，仅作历史设计记录；当前工程结构以本文件下面的"当前工程结构"为准。**
3. 源码入口：`cmd\exporter\main.go`（Exporter）、`cmd\collector\main.go`（Collector）
4. 部署与配置：`DEPLOY.md`、`config.json`、`config.collector.example.json`、`promql.md`、`grafana-dashboard.json`

## 当前工程结构（单一 module，双入口，边界隔离）

```
根目录  module ops-exporter（唯一 go.mod / go.sum，无 go.work）
├── cmd\
│   ├── exporter\main.go               Exporter 入口（ops_job_* + jenkins_build_*，HTTP 9105）
│   └── collector\main.go              Collector 入口（拉 Jenkins → POST Exporter）
├── internal\
│   ├── exporter\                      Exporter 私有边界（collector 不 import）
│   │   ├── config\                    配置（含 http 超时 + ingest 过期段）
│   │   ├── model\                     TaskResult / BuildResult + 校验
│   │   ├── reader\                    文件读取
│   │   ├── refresher\                 定时刷新 + 去重 + staleness
│   │   ├── metrics\                   ops_job_* + jenkins_build_* 指标与转换
│   │   ├── store\                     BuildStore 内存存储（去重/过期）
│   │   ├── ingest\                    POST /api/v1/ingest/build handler
│   │   └── expiry\                    TTL 过期清理（删 Gauge 保 Counter）
│   └── collector\                     Collector 私有边界（exporter 不 import）
│       ├── config\                    配置（含 state_file + 环境变量认证）
│       ├── model\                     BuildResult 契约（与 exporter/model 字段一致）
│       ├── jenkins\                   Jenkins REST API Client（Basic Auth）
│       ├── parser\                    console log 失败阶段/错误行解析
│       ├── sender\                    HTTP POST + 重试（4xx 不重试）
│       ├── collector\                 ticker 采集编排 + 去重
│       └── state\                     seen 断点续传（原子落盘）
├── docs\task1-architecture.md
├── Dockerfile.exporter / Dockerfile.collector / docker-compose.yml
├── config.docker.json / config.collector.docker.json
├── DEPLOY.md / promql.md / grafana-dashboard.json / config.json
└── dist\ops-exporter-linux-amd64      ⚠️ 拆包前编译的旧二进制，需重新构建才含 ingest/超时/过期
```

**边界约定（合并后必须保持）**：
- 只有一个 `module ops-exporter`；`cmd/exporter` 与 `cmd/collector` 是两个独立 `package main`，分别构建出两个二进制。
- `internal/exporter/*` 与 `internal/collector/*` 互不 import，各自只被对应 `cmd` 使用。
- 两侧 `model.BuildResult` 是**契约镜像**（字段/JSON 一致），故意不抽公共包，避免边界糊掉；改字段必须两边同步。
- Collector 依赖侧仍为零第三方；prometheus/client_golang 只被 exporter 侧使用（同 module 共享 go.mod，这是唯一共享点）。

## 项目背景（摘要）

- **单一 module、两条平行管道**：
  - 文件管道（原有，`ops_job_*`）：读任务结果 JSON → 指标。**零改动**。
  - 推送管道（新增，`jenkins_build_*`）：`POST /api/v1/ingest/build` → `model.ParseBuildResult` 校验 → `store.BuildStore.Save` 内存去重（map[job]最新 Build + Mutex，旧/重复 build_number 拒绝）→ `metrics.BuildMetrics.Update` 立即写指标（不等 ticker）。
- **Jenkins Collector**：定时拉取 Jenkins REST API（查 `lastCompletedBuild`，运行中的构建不阻塞）→ 结构化数据 + console log 解析 → 标准化成与 Exporter POST 契约一致的 BuildResult → HTTP 推送。带**心跳重发**（`resend_interval` 默认 2m）：周期性重发各 Job 缓存的最新 Build，Exporter 正常时幂等忽略、重启后自动恢复 Gauge（见 2026-11-24 现场问题）。

## 已完成工作（Task 1~5 + 合并单 module，全部完成）

- **Task 1**：架构分析与方案定稿（见 task1-architecture.md）。
- **Task 2**：Exporter 推送管道。HTTP 接收、校验、内存存储、指标、接线、配置向后兼容，全部完成并端到端验证。
- **Task 3**：Jenkins Collector 全部六项功能（API Client、Build 采集、日志解析、数据标准化、发送、定时采集）。端到端联调打通：mock Jenkins → 真实 Collector → 真实 Exporter → `/metrics` 指标正确，去重生效。
- **工程化**：cmd/internal 标准布局（用户明确选择"完整标准布局"，旧文档里"不拆 package"的规则**已作废**）。
- **合并单 module**：原 `jenkins-collector` 独立 module + 根 `go.work` 合并为单一 `ops-exporter` module。两个入口保留为 `cmd/exporter`、`cmd/collector`；内部实现收敛到 `internal/exporter/*` 与 `internal/collector/*` 两个互不相交的子树。删除 `go.work` 与子 `go.mod`，两侧 model 保持契约镜像不共用包。
- **Task 4**：Dashboard 新增 Jenkins 区块（20 面板 / 3 变量，独立 `$jenkins_job`）；promql.md 补 Jenkins 查询与告警；DEPLOY.md 补验证清单。用官方 `promql/parser` 校验 27 条表达式 0 invalid，curl 抓 `/metrics` 值断言全对。
- **Task 5**：可靠性 + 容器化。HTTP Server 全套超时（防 Slowloris）；数据过期（BuildStore.Prune + 删 Gauge 序列，保留 Counter）；Collector 断点续传（state 原子落盘 + 重启恢复）；`Dockerfile.exporter` / `Dockerfile.collector` + docker-compose 编排（当时为四服务）+ Prometheus/Grafana provisioning；后续按 Rancher 内置栈改为 compose 只含 exporter + 可选 collector（prometheus/grafana 服务已彻底删除）。
- **总计约 122 个测试全绿**（合并后单 module 一次 `go test ./...` 覆盖全部 15 个包）；两个入口均可独立 `go build`。

## 环境与规则

- Windows；Go 在 `D:\Go\bin`（沙箱 PATH 没有，需先 `export PATH="/d/Go/bin:$PATH"`）；go.mod 声明 go 1.26.0。
- **验证命令（合并后：单 module 一次跑完全部）**：
  ```bash
  export PATH="/d/Go/bin:$PATH"
  cd "C:/Users/shironeko/WorkBuddy/2026-11-18-16-36-15"
  go vet ./... && go test ./... -count=2
  go build ./cmd/exporter && go build ./cmd/collector   # 两个入口均可独立构建
  ```
- 规则：每步全量测试；单文件单职责；每个功能必须有测试；不许只编译不测试；完成后按固定格式汇报（修改文件 / 为什么 / 核心逻辑 / 测试结果 / 已知问题 / 下一步前置条件）。
- 依赖规则：Collector 代码保持零第三方 import；Exporter 仅用已有的 prometheus/client_golang，不引新依赖（曾因 `prometheus/testutil` 引入新依赖而移除）。合并后共享同一 go.mod，不得因合并给 collector 侧引入第三方 import。
- 每完成一个实质性步骤，向 `.workbuddy\memory\2026-11-18.md` 追加记录。
- 部署背景：Linux k3s-node1 (192.168.193.147) 上有一份源码变体在跑（与仓库不一致），Rancher Prometheus 抓取 job="ops-exporter" 已通；Grafana dashboard JSON 在仓库根目录。
- **Jenkins 环境**：容器化 Jenkins 在 `192.168.193.147:8081`（v2.555.3，本机实测可达），配置已指向它，Job 为中文名 `构建任务` / `部署任务`；**强制认证**，匿名 `/api/json` 403，必须提供 JENKINS_USERNAME + JENKINS_API_TOKEN 才能采集；同机 9105 exporter 已起（需确认是否仓库版本）。本环境无 Docker/SSH 访问权限，无法自动取容器内凭据。
- **现场问题（2026-11-23）**：目标机 `/metrics` 无 `jenkins_build_*`。排查发现截图里两个 Job 都在执行中，旧版 Collector 只查 `lastBuild`（`result=null` 跳过）导致完全无数据。**已修复**：采集端点改用 `lastCompletedBuild`（`internal/collector/jenkins/client.go`），并新增回归测试；DEPLOY.md 第 8.1 节新增四步排查清单。目标机需重新构建/更新 Collector 镜像或二进制才会生效。
- **现场问题（2026-11-24）**：Grafana 文件管道全 No data 且「Exporter 新鲜度 56.9 years」。确认目标机 9105 为**容器**启动、结果数据目录是宿主机 `/data/log`。**已切换**：`config.docker.json` 的 `result_dir` 改为 `/data/log`，compose 以只读绑定挂载 `/data/log:/data/log:ro` 替代原 `exporter-data` named volume（collector 的 `collector-state` 卷不变）。本地用仓库样例数据（含旧命名 `database_backup`/`file_sync`）冒烟验证：`ops_job_*` 四组指标与 `ops_exporter_last_refresh_timestamp_seconds` 均正常产生，格式兼容旧变体无需改名。目标机需同步仓库（含 `vendor/`）并 `docker compose up -d --build exporter` 重建后才生效；Dashboard 的 No data 配色/新鲜度细调也需重新导入 `grafana-dashboard.json`。
- **现场问题（2026-11-24 11:18）**：重启 exporter 修文件管道后，Jenkins 面板"一半有数据"——Counter 类（构建总数 5.02 / 失败数 4.02 / 趋势）有值，Gauge 类（构建状态 / 最近耗时 / 成功率 / 距今）全 No data。根因：Exporter 内存 BuildStore 重启清空，Collector 断点 `state.json` 认为 build 已处理不再发。**已修复**：Collector 加心跳重发 `resend_interval`（默认 2m，`internal/collector/collector/collector.go` 的 `latestBuild`/`lastResendAt`/`dueForResend`；config.go 校验 + main.go 接线 + 两个 config json），6 个新测试覆盖。重发靠 Exporter build_number 幂等，正常时 ignored 不重复计数，重启后 ≤2 分钟恢复。目标机需重新构建 Collector 镜像生效。
- **Rancher 环境**：入口 `https://rancher.my.org/dashboard/c/local/monitoring`（local 集群 Monitoring）；Prometheus 与 Grafana 均走 Rancher 代理，无独立 9090/3000 端口。查询接口形如 `https://rancher.my.org/api/v1/namespaces/cattle-monitoring-system/services/http:rancher-monitoring-prometheus:9090/proxy/api/v1/query?query=...`（需 Rancher Bearer Token）。本会话开发环境无法访问 rancher.my.org，相关操作须在可访问 Rancher 的网络内执行。

## 关键契约与注意事项

- **POST 契约**：`POST /api/v1/ingest/build`，Body 字段 `job_name`(必填) / `build_number`(必填,>0) / `status`(必填: success|failed|unstable|aborted) / `duration`(≥0) / `timestamp`(秒, 不超过 now+5min) / `failed_stage`(选填)。非 POST → 405 + `Allow: POST`；非法 → 400 `{"error":...}`；重复/旧 build → 200 `{"status":"ignored"}` 且**不写指标**。
- **指标**：`jenkins_build_status/duration/last_timestamp`（GaugeVec）、`jenkins_build_total/failure_total`（CounterVec），仅 `job_name` 标签；`failure_total` 在首次非失败时也要建 0 序列，否则 Grafana 显示 No data。
- **拆包维护提示**：`metrics.go + updater.go + buildmetrics.go + buildupdater.go` 必须同包（方法依附类型，现均在 `internal/exporter/metrics`）；跨包测试 helper 用"类型别名 + 每包 `testhelpers_test.go` 副本"解决；`staleness_test.go` 归在 `internal/exporter/refresher`。
- **Collector 认证**：JENKINS_USERNAME / JENKINS_API_TOKEN 环境变量覆盖，绝不硬编码。
- **Task 5 配置段**（均可选、向后兼容）：
  - Exporter `config.json` 的 `http` 段：`read_header_timeout`(5s)/`read_timeout`/`write_timeout`/`idle_timeout`/`shutdown_timeout`/`max_header_bytes`——超时防 Slowloris。
  - Exporter `ingest` 段新增 `max_build_age`（如 `168h`，默认 `0`=禁用过期）与 `expiry_interval`（默认 `5m`）；过期只删 3 个 Gauge 序列，`total`/`failure_total` 两个 Counter 保留。
  - Collector `config.json` 的 `state_file`（空=禁用持久化）：seen 断点原子落盘，重启不重发已处理 Build；损坏降级为空（Exporter 幂等兜底）。
- **容器化（合并后）**：根 `docker-compose.yml`；`Dockerfile.exporter` 与 `Dockerfile.collector` 的构建 context **均为仓库根目录**（共享 go.mod），分别只编译 `./cmd/exporter` 与 `./cmd/collector`；配置文件为根 `config.docker.json` 与 `config.collector.docker.json`（原 `jenkins-collector/` 下同名文件已上移）；认证走 `.env`，不进镜像。
- **监控栈复用 Rancher（2026-11-23）**：目标 k3s 由 Rancher 管理且已内置 Prometheus + Grafana。**compose 已彻底删除 `prometheus`/`grafana` 服务**（非 profile 保留），只含 `exporter`（默认）+ `collector`（`collector` profile）。`docker compose up -d` 起 exporter，Rancher 按 DEPLOY.md 第 5 节配 Additional Scrape Configs 抓 `192.168.193.147:9105`、第 6 节导入 Dashboard 到 Rancher Grafana。`deploy/prometheus/` 与 `deploy/grafana/provisioning/` 仅作独立部署参考样例，compose 不引用。
- **离线构建（2026-11-23 修复）**：依赖已 `go mod vendor` 固化到根 `vendor/`（约 12M），两个 Dockerfile 设 `GOFLAGS=-mod=vendor` 且删除 `RUN go mod download`，构建不访问 Go 模块代理。**起因**：在 192.168.193.147 内网构建时 `go mod download` 报 `exit code: 1`（本地同一 go.mod/go.sum 下载正常，判定为容器无外网/代理不可达）。已用 `GOPROXY=off GOFLAGS=-mod=vendor` 本地模拟验证双入口可构建。**维护**：增删依赖后必须重跑 `go mod vendor`；传输仓库到构建机时 `vendor/` 不可省略。
- **注意**：本会话环境 Docker 不可用、无 PyYAML，compose 仅做静态一致性校验（路径/COPY 源/旧引用检查全通过），未实际 `docker compose up`。

## 下一步候选（待用户选择）

1. **部署到 k3s-node1 做真实环境联调**（推荐）：按 DEPLOY.md 第 11 节 `docker compose up -d`，接真实 Jenkins 验证全链路。
2. 重新构建 `dist\` 二进制（当前那份是拆包前的旧版，不含 ingest/超时/过期；合并后需按新路径 `./cmd/exporter`、`./cmd/collector` 构建）。
3. 同步 `docs\task1-architecture.md` 中已过时的源码路径说明（合并为单 module 后差异进一步扩大）。
4. 若 Rancher Prometheus 可用，按 DEPLOY.md 第 8 节做真实 PromQL 查询复核（Task 4 因本地无 Prometheus 未能执行真实抓取）。
