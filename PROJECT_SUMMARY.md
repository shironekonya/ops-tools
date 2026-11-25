# Ops Exporter 项目总结与升级拓展预留

> 基线：2026-11-25  
> 状态：**主体功能基本完成**，进入收尾验证与后续扩展规划阶段。  
> 当前方案：单一 Go module、双入口（exporter / collector）、复用 Rancher 内置 Prometheus / Grafana / Alertmanager，不自建监控告警栈，不使用 alertai。

---

## 1. 项目定位

Ops Exporter 把两类数据统一成 Prometheus 指标：

| 管道 | 数据来源 | 指标前缀 | 状态 |
|---|---|---|---|
| 文件管道 | 任务结果 JSON（`/data/log/*.json`） | `ops_job_*`、`ops_exporter_*` | 已完成 |
| 推送管道 | Jenkins Collector → `POST /api/v1/ingest/build` | `jenkins_build_*` | 已完成 |

监控告警完全复用目标 k3s 的 Rancher Monitoring：

```text
Exporter / Collector
        │ /metrics
        ▼
Rancher Prometheus 抓取
        │ recording / alerting rules
        ▼
ALERTS 指标
        │
        ▼
Rancher Alertmanager（severity 路由 + 抑制）
        │
        ▼
原生 email / webhook receiver
```

本仓库不含 Prometheus / Grafana / Alertmanager 服务，也不含自建通知服务。

---

## 2. 已完成能力

### 2.1 代码与架构

- 单一 `module ops-exporter`，两个独立入口：
  - `cmd/exporter`：HTTP `:9105`，暴露 `/healthz`、`/metrics`、`POST /api/v1/ingest/build`；
  - `cmd/collector`：定时拉 Jenkins REST API，推送标准化 BuildResult 到 Exporter。
- 边界隔离：`internal/exporter/*` 与 `internal/collector/*` 互不 import。
- `BuildResult` 在两侧为契约镜像，字段/JSON 一致，不共用包。
- Collector 保持零第三方依赖；Exporter 只使用 `prometheus/client_golang`。
- `vendor/` 已固化依赖，支持离线构建。

### 2.2 Exporter

- 文件管道：目录扫描、坏文件隔离、定时刷新、日志降噪。
- 推送管道：BuildStore 内存去重（按 `job_name + build_number`）、立即写指标。
- 可靠性：HTTP 全套超时、优雅退出、`last_refresh` 新鲜度指标。
- 过期清理：`max_build_age` 只删 Gauge 序列，保留 Counter。
- 指标：
  - `ops_job_last_status`
  - `ops_job_last_run_timestamp_seconds`
  - `ops_job_last_success_timestamp_seconds`
  - `ops_job_last_duration_seconds`
  - `ops_exporter_last_refresh_timestamp_seconds`
  - `jenkins_build_status`
  - `jenkins_build_duration_seconds`
  - `jenkins_build_last_timestamp_seconds`
  - `jenkins_build_total`
  - `jenkins_build_failure_total`

### 2.3 Collector

- 使用 `lastCompletedBuild` 采集已完成构建，避免运行中构建导致无数据。
- console log 解析失败阶段与错误行。
- Basic Auth 凭据走 `JENKINS_USERNAME` / `JENKINS_API_TOKEN` 环境变量。
- `state.json` 原子落盘，重启不重发已处理构建。
- 心跳重发（默认 2m）：Exporter 重启内存清空后自动恢复 Gauge，正常时幂等忽略。

### 2.4 Dashboard / PromQL / 告警

- `grafana-dashboard.json`：文件管道 + Jenkins 区块，20 面板 / 3 变量。
- `promql.md`：查询、告警规则与排障表达式。
- `deploy/prometheus/rules/recording_rules.yml`：10 条记录规则。
- `deploy/prometheus/rules/alerting_rules.yml`：10 条告警规则。
- 告警分级：critical / warning / info 三种通知节奏。
- 抑制规则：Exporter / Collector 停摆时抑制失真下游告警。

### 2.5 部署与告警配置

- `docker-compose.yml`：只含 `exporter` + 可选 `collector` profile。
- `DEPLOY.md`：Linux 二进制 / systemd / Rancher 抓取 / Dashboard / Helm 升级。
- `ALERTING_SETUP.md`：告警全链路配置、排障、webhook 测试方法。
- Helm values：
  - `deploy/rancher-monitoring-values.yaml`：邮件版；
  - `deploy/rancher-monitoring-values-webhook.yaml`：webhook 版。
- Alertmanager 原生配置：
  - `deploy/alertmanager/alertmanager.yml`：邮件版；
  - `deploy/alertmanager/alertmanager-webhook.yml`：webhook 版。
- 已确认关键层级：
  - 原生 Alertmanager 配置放 `alertmanager.config`；
  - 不能放 `alertmanager.alertmanagerSpec.additionalConfigString`；
  - `alertmanager.secret.recreateIfExists: true` 用于覆盖 Rancher 默认不会更新的 Secret。

### 2.6 测试与验证

- 项目累计约 150 个测试用例。
- 合并单 module 后曾执行 `go vet ./...` 与 `go test ./... -count=2` 全绿。
- 两个入口均可独立 `go build`。
- 告警规则、Dashboard PromQL、Alertmanager YAML 均做过解析校验。
- webhook values 用本地 `helm template` 渲染确认：
  - 无 `spec.global / spec.route / spec.receivers / spec.inhibit_rules` 未知字段；
  - Secret 内含正确的 receiver 与 2 条抑制规则。

---

## 3. 当前现场状态与待确认项

> 以下不是代码缺陷，而是目标机/Rancher 上的收尾确认事项。

| 项目 | 当前状态 | 待确认 / 下一步 |
|---|---|---|
| <K3S_NODE> 上的 Exporter | 已有服务在跑，但源码可能不是仓库最新版 | 同步仓库（含 `vendor/`）并重建，确认含 ingest / 超时 / 过期逻辑 |
| Jenkins Collector | 代码已完成，需 profile 启动 | 填 `.env` 凭据，`docker compose --profile collector up -d --build` |
| 文件管道 | 配置已切到 `/data/log` | 确认容器只读挂载 `/data/log:/data/log:ro` |
| Rancher Prometheus 抓取 | 已通 | 复核 `ops_job_*` 与 `jenkins_build_*` 是否都能查到 |
| Grafana Dashboard | JSON 已修正 | 重新导入 `grafana-dashboard.json`，确认 No data 语义 |
| Alertmanager 配置层级 | 已修正为 `alertmanager.config` | 现场重新 `helm upgrade` 或直接 patch Secret |
| webhook 接收链路 | 集群内 curl 测试返回 200；Alertmanager API 返回 200 | **尚未确认本地 webhook 终端打印 JSON**；需确认 Secret receiver 为 `webhook-ops` 并等待 group_wait |
| `dist/ops-exporter-linux-amd64` | 拆包前旧二进制 | 不含 ingest / 超时 / 过期逻辑，需按新入口重新构建 |
| `docs/task1-architecture.md` | 保留历史设计原文 | 目标路径已过时，仅作历史记录，以 `docs/ARCHITECTURE.md` 的目录结构为准 |

---

## 4. 收尾验证清单

建议按这个顺序完成现场收尾：

1. **确认 Alertmanager 实际配置**

   ```bash
   kubectl -n cattle-monitoring-system get secret \
     alertmanager-rancher-monitoring-alertmanager \
     -o jsonpath='{.data.alertmanager\.yaml}' | base64 -d \
     | grep -nE 'receiver:|webhook_configs|email_configs'
   ```

   必须看到 `receiver: webhook-ops`。

2. **patch Secret（如仍为 email-ops）**

   ```bash
   kubectl -n cattle-monitoring-system create secret generic \
     alertmanager-rancher-monitoring-alertmanager \
     --from-file=alertmanager.yaml=alertmanager-webhook.yml \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

   等待 1~2 分钟让 Alertmanager 重载。

3. **发一条直接告警**

   ```bash
   kubectl -n cattle-monitoring-system port-forward \
     svc/rancher-monitoring-alertmanager 19094:9093 &

   curl -i --max-time 10 -XPOST \
     http://127.0.0.1:19094/api/v2/alerts \
     -H 'Content-Type: application/json' \
     -d '[{"labels":{"alertname":"TestWebhook","severity":"critical","service":"ops-exporter"},"annotations":{"summary":"webhook link test"}}]'
   ```

   等待 10~20 秒，本地 webhook 终端应打印 JSON。

4. **确认 Prometheus 侧告警**

   ```promql
   ALERTS{alertstate="firing"}
   jenkins_build_status
   ```

5. **重建并同步二进制**

   ```bash
   export PATH="/d/Go/bin:$PATH"
   cd "C:/Users/shironeko/WorkBuddy/2026-11-18-16-36-15"
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
     go build -o dist/ops-exporter-linux-amd64 ./cmd/exporter
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
     go build -o dist/ops-collector-linux-amd64 ./cmd/collector
   ```

---

## 5. 升级与拓展预留

> 以下为后续候选，不在当前收尾阶段实施。

### P0：现场稳定性

| 方向 | 说明 | 前置条件 |
|---|---|---|
| 确认 webhook 通知落地 | 完成第 4 节清单，看到 JSON 后视为链路闭环 | Secret receiver 为 `webhook-ops` |
| 同步目标机源码 | 避免源码变体导致排查结论偏差 | 传输仓库时包含 `vendor/` |
| 重新导入 Dashboard | 修复假绿 / No data / 表格列名问题 | Rancher Grafana 可访问 |
| 重建 dist 二进制 | 当前旧版不含新增能力 | Go 1.26 + 本地 `vendor/` |

### P1：可观测性增强

| 方向 | 说明 |
|---|---|
| Jenkins 队列指标 | 采集 queue size / blocked / pending 时间，提前发现构建堆积 |
| 运行中构建指标 | 当前只采 `lastCompletedBuild`；可在不影响去重的前提下增加运行态 Gauge |
| 失败阶段维度 | 把 `failed_stage` 提为 label，支持按阶段统计失败率 |
| 构建成功率 SLO | 基于 Counter 做 7d / 30d 成功率与错误预算告警 |
| 告警抑制细化 | 按 `job_name` / `service` 维度细化 inhibit，避免跨任务误抑制 |

### P2：可靠性与数据模型

| 方向 | 说明 |
|---|---|
| BuildStore 持久化 | 当前重启依赖 Collector 心跳恢复；可改为本地 bolt/wal 或定期快照 |
| ingest 认证 | 增加 token / mTLS，避免集群内任意 Pod 推送伪造构建数据 |
| 背压与限流 | 对 `/api/v1/ingest/build` 加并发限制与请求体上限 |
| state schema 版本 | Collector `state.json` 增加 version 字段，便于未来字段迁移 |
| 多 Exporter 实例 | 若做 HA，需要明确 BuildStore 是每实例独立还是共享状态 |

### P2：通知与路由扩展

| 方向 | 说明 |
|---|---|
| 自定义邮件 Subject | 必须按 Helm `tpl` 规则转义花括号，或使用 `alertmanager.templates` 模板文件 |
| 按团队路由 | 在规则 label 增加 `team`，Alertmanager 用 `matchers` 分流 |
| 多通知渠道 | 企业微信 / 钉钉 / 飞书 webhook；仍使用 Alertmanager 原生 webhook receiver |
| 静默管理 | 用 Alertmanager `amtool` 或 UI 做计划维护静默，避免改配置 |
| AlertmanagerConfig CRD | 只有需要 namespace 级隔离时才考虑；不要与 `additionalConfigString` 混用 |

### P3：平台化

| 方向 | 说明 |
|---|---|
| 独立 Helm chart | 把 Exporter / Collector 做成 chart，values 管理镜像、Job 列表、资源限制 |
| ServiceMonitor / PodMonitor | 用 CRD 替代 static scrape config，更符合 Prometheus Operator 习惯 |
| 多集群标签 | 在 scrape / 规则中注入 `cluster` label，支持多 k3s 汇总 |
| 统一日志与追踪 | slog 结构化日志增加 `trace_id` / `request_id`，便于跨组件排障 |

---

## 6. 维护原则

继续开发时必须保持这些约束：

1. **不引入监控栈重复部署**：Prometheus / Grafana / Alertmanager 一律复用 Rancher。
2. **保持双入口边界**：`cmd/exporter` 与 `cmd/collector` 独立构建，`internal/exporter` 与 `internal/collector` 互不 import。
3. **契约镜像同步**：两侧 `BuildResult` 字段或 JSON tag 变化时，必须同时修改并补测试。
4. **Collector 零第三方依赖**：不要把 Exporter 侧依赖带回 Collector。
5. **依赖变更必须重跑 `go mod vendor`**：否则容器离线构建失败。
6. **每个新功能必须有测试**：优先用 `httptest` / `t.TempDir` / fake Jenkins，不依赖真实集群。
7. **告警配置单一来源**：以 `deploy/rancher-monitoring-values*.yaml` 为准，修改后同步源 YAML 与文档。
8. **避免过早抽象**：只有出现真实的多实现或多消费者时再抽公共包。

---

## 7. 相关文档索引

| 文档 | 用途 |
|---|---|
| `README.md` | 项目总览、架构图、数据流向图、文档入口 |
| `docs/ARCHITECTURE.md` | 目录结构、核心机制、边界约定、指标清单 |
| `docs/QUICKSTART.md` | 容器启动、本地验证、配置文件说明 |
| `DEPLOY.md` | Linux 二进制 / systemd / Docker / Rancher 抓取 / Dashboard 导入 / Helm 升级 |
| `ALERTING_SETUP.md` | 告警规则、Alertmanager 配置、webhook 测试与排障 |
| `promql.md` | 查询语句、告警表达式、排障 PromQL |
| `docs/task1-architecture.md` | 动态化改造 Task1 架构分析（历史设计文档，路径已过时） |
| `PROJECT_SUMMARY.md` | 本文件：当前状态与升级拓展路线 |

---

## 8. 一句话结论

代码与配置主体已经完成；当前唯一闭环动作是**在 k3s / Rancher 现场确认 Alertmanager 已使用 webhook receiver 并收到测试 JSON**。完成后，后续工作应从 P0「同步目标机源码 / 重建二进制 / 重新导入 Dashboard」开始，再按 P1~P3 逐步扩展。
