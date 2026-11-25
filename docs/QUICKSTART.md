# 快速开始

> 从原 `README.md` 拆分。面向首次启动、本地验证和配置文件调整的读者。
> 项目总览见 `README.md`；完整 Linux/systemd 与 Rancher 侧配置见 `DEPLOY.md`。

## 1. 容器部署

目标机准备：安装 Docker + Docker Compose，仓库放到部署目录。

```bash
cd /data/jenkins-exporter

# 只起 Exporter（文件管道）
docker compose up -d

# 需要 Jenkins 采集时：先 cp .env.example .env 并填 JENKINS_API_TOKEN
docker compose --profile collector up -d --build

curl -s localhost:9105/healthz
curl -s localhost:9105/metrics | grep -E '^ops_job_|^jenkins_build_'
```

- Exporter 只读挂载宿主机 `/data/log`（任务结果 JSON 所在目录）。
- Collector 断点存于 `collector-state` 卷，容器重建后不重复推送。
- 告警规则与通知路由在 Rancher Monitoring 中配置（见 `DEPLOY.md` 第 5、6、6.1、6.2 节），无独立容器。

Dashboard 导入：把 `grafana-dashboard.json` 导入 Rancher Grafana，数据源选内置 Prometheus。

## 2. 本地开发与验证

```bash
# Windows 本机 Go 路径（按实际调整）
export PATH="/d/Go/bin:$PATH"

go vet ./...
go test ./... -count=2
go build ./cmd/exporter && go build ./cmd/collector

# 离线构建（与 Dockerfile 一致）
GOFLAGS=-mod=vendor go build ./cmd/exporter
```

增删依赖后必须重跑 `go mod vendor`，否则容器离线构建会失败。

## 3. 配置文件

| 文件 | 用途 |
|---|---|
| `config.json` / `config.docker.json` | Exporter：监听地址、`result_dir`、刷新周期、`ingest` 过期、`http` 超时 |
| `config.collector.example.json` / `config.collector.docker.json` | Collector：Jenkins 地址、Job 列表、Exporter 地址、`state_file`、`resend_interval` |
| `.env` | `JENKINS_USERNAME` / `JENKINS_API_TOKEN`（复制 `.env.example`，不进版本库与镜像） |

关键字段：

- `result_dir`：任务结果 JSON 目录，容器为 `/data/log`。
- `jobs`：Jenkins Job 名称列表（如 `构建任务` / `部署任务`）。
- `resend_interval`：心跳重发间隔，默认 `2m`；`0s` 禁用心跳。
- `max_build_age`：构建数据过期时间，默认 `168h`，只删 Gauge 保留 Counter。

SMTP 凭据在 Rancher Alertmanager 配置中设置，不放在本仓库。

## 4. 任务结果 JSON 格式

文件放在 `result_dir` 下，一个文件一个对象：

```json
{
  "job_name": "file_sync",
  "status": "success",
  "start_time": "2026-11-20T15:21:20+08:00",
  "end_time": "2026-11-20T15:25:50+08:00",
  "exit_code": 0,
  "message": "sync completed"
}
```

`data/` 目录内有可直接使用的样例；坏文件会被隔离并记日志，不影响其它任务。
