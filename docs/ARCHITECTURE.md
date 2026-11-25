# 架构与工程结构

> 从原 `README.md` 拆分。面向需要理解代码结构、扩展功能或排查边界问题的读者。
> 总览与图见 `README.md`；部署步骤见 `DEPLOY.md`；告警配置见 `ALERTING_SETUP.md`。

## 1. 目录结构

```text
cmd/
  exporter/         Exporter 入口（HTTP 9105）
  collector/        Collector 入口（Jenkins → Exporter）
internal/
  exporter/         config / model / reader / refresher / metrics / store / ingest / expiry
  collector/        config / model / jenkins / parser / sender / collector / state
vendor/             离线构建依赖（go mod vendor 生成，传输仓库时不可省略）
config*.json        本地与容器配置样例
data/               任务结果 JSON 样例（可直接在 result_dir 使用）
docker-compose.yml  Dockerfile.exporter / Dockerfile.collector
grafana-dashboard.json  Grafana 面板（10 个）
deploy/prometheus/rules/   告警规则 alerting_rules.yml + 记录规则 recording_rules.yml
deploy/alertmanager/       原生 Alertmanager 配置（邮件版 / webhook 版）
deploy/rancher-monitoring-values*.yaml   Helm values（规则 + Alertmanager 配置）
docs/                架构与快速开始文档
dist/               预编译 linux/amd64 二进制（本地产物，已在 .gitignore 中排除）
```

部署与告警配置的**唯一权威来源**是 `deploy/` 目录和 Helm values 文件；文档中的示例需与它们保持同步。

## 2. 边界约定

- 两个 `internal/*` 子树互不 import。
- `BuildResult` 在 exporter / collector 两侧为契约镜像：字段与 JSON tag 一致但不共用包，改字段必须同步两边并补测试。
- Collector 保持零第三方依赖；Exporter 只使用 `prometheus/client_golang`。
- 告警通知完全交给 Rancher 内置 Alertmanager，本仓库不含自建通知服务、不部署 Prometheus / Grafana。
- 增删依赖后必须重跑 `go mod vendor`，否则容器离线构建失败。

## 3. 核心机制

| 机制 | 说明 |
|---|---|
| 双管道指标 | `ops_job_*`（任务结果）、`jenkins_build_*`（构建结果） |
| 幂等去重 | 按 `job_name + build_number`，旧/重复构建返回 `ignored` 且不写指标 |
| 断点续传 | Collector 的 `state.json` 原子落盘，重启不重发已处理构建 |
| 心跳重发 | 每 `resend_interval`（默认 2m）重发各 Job 最新构建；Exporter 重启内存清空后自动恢复 Gauge，正常时幂等忽略 |
| 数据过期 | `max_build_age`（默认 168h）只删 Gauge 序列，Counter 保留 |
| 可靠性 | HTTP 全套超时防 Slowloris；Collector 认证走环境变量，不进镜像 |
| 告警规则 | 10 条告警规则产生 `ALERTS` 指标；10 条记录规则预计算口径，面板与告警共用 |
| 告警路由 | Alertmanager 按 severity 分级（critical/warning/info 不同节奏）+ 抑制级联告警 |

## 4. 指标清单

### 任务管道（文件）

| 指标 | 含义 |
|---|---|
| `ops_job_last_status` | 最近一次执行状态（1 成功 / 0 失败） |
| `ops_job_last_run_timestamp_seconds` | 最近一次结束时间（Unix 秒） |
| `ops_job_last_success_timestamp_seconds` | 最近一次成功结束时间 |
| `ops_job_last_duration_seconds` | 最近一次执行耗时 |
| `ops_exporter_last_refresh_timestamp_seconds` | 最近一次成功刷新时间（无标签，用于检测 exporter 自身失效） |

### 构建管道（Jenkins 推送）

| 指标 | 含义 |
|---|---|
| `jenkins_build_status` | 最近一次构建状态（1 成功 / 0 失败） |
| `jenkins_build_duration_seconds` | 最近一次构建耗时 |
| `jenkins_build_last_timestamp_seconds` | 最近一次构建时间戳 |
| `jenkins_build_total` | 累计构建数（Counter） |
| `jenkins_build_failure_total` | 累计失败数（Counter） |

### 告警侧

- `ALERTS{alertstate="firing"}`：由 `deploy/prometheus/rules/alerting_rules.yml` 产生。
- `ops_exporter:rules_loaded:bool`：加载标记，用于确认规则已生效。
- 面板与 No data 显示语义见 `grafana-dashboard.json`；Dashboard 底部两个面板直接展示 `ALERTS`。

查询语句、阈值设定和排障 PromQL 见 `promql.md`。
