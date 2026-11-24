# Ops Exporter

将任务执行结果和 Jenkins 构建结果统为 Prometheus 指标，供 Rancher Prometheus 抓取、Grafana 展示。

单一 Go module（`ops-exporter`），两个独立入口：Exporter（:9105）和 Collector。

## 架构

```
任务结果 JSON (/data/log/*.json)          Jenkins REST API
        |                                      |
        v                                      v
   reader -> refresher                   Collector -> POST /api/v1/ingest/build
        |                                      |
        +------------------+-------------------+
                           v
              Exporter :9105 -> /metrics -> Prometheus -> Grafana
```

- Collector 可选——无 Jenkins 时只跑 Exporter，文件管道正常工作。
- Prometheus / Grafana 由目标 k3s 的 Rancher Monitoring 提供，本仓库只编排业务容器。

## 目录结构

```
cmd/exporter/  cmd/collector/          # 两个入口
internal/exporter/                     # 读取/刷新/指标/存储/接收/过期
internal/collector/                    # Jenkins 采集/解析/发送/状态
vendor/  dist/  config*.json  docker-compose.yml
grafana-dashboard.json  promql.md  DEPLOY.md  docs/
```

`internal/exporter` 与 `internal/collector` 互不 import，两侧 `BuildResult` 为契约镜像，改字段须同步。

## 核心机制

- **双管道指标**：`ops_job_*`（任务）、`jenkins_build_*`（构建）
- **幂等去重**：按 `job_name + build_number`，重复构建忽略
- **断点续传**：Collector `state.json` 原子落盘，重启不重发
- **心跳重发**：默认每 2m 重发最新构建，Exporter 重启后自动恢复 Gauge
- **数据过期**：`max_build_age`（默认 168h）只清 Gauge，Counter 保留
- **安全**：HTTP 全套超时；认证走环境变量，不进镜像

## 快速开始

```bash
# 容器部署（目标机）
cd /data/jenkins-exporter
docker compose up -d                                    # 仅 Exporter
# 需要 Jenkins：填 .env 的 JENKINS_API_TOKEN 后
docker compose --profile collector up -d --build         # + Collector

curl -s localhost:9105/healthz
curl -s localhost:9105/metrics | grep -E '^ops_job_|^jenkins_build_'
```

```bash
# 本地开发
go vet ./... && go test ./... -count=2
go build ./cmd/exporter && go build ./cmd/collector
GOFLAGS=-mod=vendor go build ./cmd/exporter              # 离线构建
```

增删依赖后必须重跑 `go mod vendor`。

## 配置

| 文件 | 用途 |
|---|---|
| `config*.json` | Exporter：监听地址、`result_dir`、刷新周期、过期、超时 |
| `config.collector.*.json` | Collector：Jenkins 地址、Job 列表、`resend_interval` |
| `.env` | `JENKINS_USERNAME` / `JENKINS_API_TOKEN`（不进版本库） |

## 指标

- 任务：`ops_job_last_status` / `last_run_timestamp` / `last_success_timestamp` / `last_duration` / `ops_exporter_last_refresh_timestamp`
- 构建：`jenkins_build_status` / `duration` / `last_timestamp` / `total` / `failure_total`
- 查询与告警规则见 `promql.md`，面板见 `grafana-dashboard.json`。

## 相关文档

- `DEPLOY.md` — 部署、验证与故障排查
- `HANDOFF.md` — 交接上下文与边界约定
- `docs/task1-architecture.md` — 架构设计
