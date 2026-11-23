# Ops Exporter Linux 部署指南

目标环境：Linux 虚拟机（k3s 集群节点同网段），Prometheus 为 Rancher Monitoring 内置实例。

## 1. 传输与安装

```bash
# Windows 侧传到 Linux（用户名/路径按实际改）
scp dist/ops-exporter-linux-amd64 user@<LINUX_IP>:/tmp/

# Linux 侧
sudo install -m 0755 /tmp/ops-exporter-linux-amd64 /usr/local/bin/ops-exporter
sudo mkdir -p /etc/ops-exporter /var/lib/ops-exporter/results
```

## 2. 配置文件 `/etc/ops-exporter/config.json`

```json
{
  "listen_addr": ":9105",
  "result_dir": "/var/lib/ops-exporter/results",
  "refresh_interval": "10s",
  "log_level": "info"
}
```

## 3. 样例任务结果（验证用，可先放一条）

```bash
cat > /var/lib/ops-exporter/results/backup.json <<'EOF'
{"job_name":"database_backup","status":"success","start_time":"2026-11-20T10:00:00+08:00","end_time":"2026-11-20T10:05:30+08:00","exit_code":0,"message":"backup completed"}
EOF
```

## 4. systemd 服务 `/etc/systemd/system/ops-exporter.service`

```ini
[Unit]
Description=Ops Exporter (task results to Prometheus metrics)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/ops-exporter -config /etc/ops-exporter/config.json
Restart=on-failure
RestartSec=5
# 配置非法/端口占用等启动失败会 exit(1)，由 Restart 策略兜底重试
DynamicUser=yes
StateDirectory=ops-exporter

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ops-exporter
curl -s localhost:9105/healthz   # {"status":"ok"}
curl -s localhost:9105/metrics | grep ops_
```

## 5. Rancher Monitoring 抓取配置

集群 → Monitoring → 定制化（或 rancher-monitoring 应用 values）→
**Additional Scrape Configs**，`<LINUX_IP>` 换成运行 exporter 的虚拟机 IP：

```yaml
- job_name: ops-exporter
  scrape_interval: 15s
  static_configs:
    - targets:
        - <LINUX_IP>:9105
      labels:
        env: lab
```

保存后等 1~2 分钟让 Prometheus 重新加载。

## 6. Grafana Dashboard 导入

Rancher 集群 → Monitoring → Grafana → Dashboards → **New → Import**，
上传 `grafana-dashboard.json`，数据源选 `PROMETHEUS`（Rancher 内置）。

Dashboard 分两个区块，共用同一数据源与 Prometheus job（`ops-exporter`），30s 自动刷新：
上半部分是**文件管道**（`ops_job_*`，任务变量 `$job`），下半部分是 **Jenkins 推送管道**（`jenkins_build_*`，独立变量 `$jenkins_job`）。

| 区块 | 内容 | 异常表现 |
|---|---|---|
| 顶部统计 | 失败任务数 / 任务总数 / exporter 新鲜度 / 抓取状态 | 失败>0 红色；新鲜度>60s 红；抓取中断红 |
| 各任务状态 | 每个任务一个格子：成功(绿)/失败(红) | 失败一目了然 |
| 距上次成功 | 绿→黄(12h)→红(24h)；"从未成功"红色 | 连续失败被掩盖时此处仍会变红 |
| 距上次执行 | 绿→黄(12h)→红(24h) | 长时间未执行/停跑任务直接红 |
| 任务总览表 | 四指标按 job_name 合并巡检 | 一行一任务 |
| 趋势图 | last_run 年龄曲线 + 耗时阶梯图 | 锯齿=正常；只涨不跌=停跑 |
| **Jenkins·统计** | 24h 构建总数 / 失败数 / 成功率 / 最近构建距今 | 失败>0 红；成功率<90% 红；距今>24h 红 |
| **Jenkins·每 Job** | 最近状态格子 + 最近耗时格子（按 `$jenkins_job` 重复） | 非成功红；耗时突增 |
| **Jenkins·趋势** | 构建耗时阶梯图 + 每小时构建频次柱状图 | 耗时突增=劣化；频次骤降=Collector 停摆 |
| **Jenkins·总览表** | 六指标按 job_name 合并：状态/耗时/距今/构建总数/失败总数/成功率 | 一行一 Jenkins Job |

> 阈值（60s 新鲜度 / 12h·24h 陈旧）与 promql.md 告警规则同源，改告警阈值时同步改面板。
>
> 导入注意：数据源变量默认 `prometheus`（Rancher 内置 Prometheus 的 uid）。若你的实例 uid 不同，
> 导入时在上方"数据源"下拉重新选择一次即可，面板会自动跟随。重复导入同一 uid 会覆盖旧版本。
>
> 两个任务变量互相独立：`$job` 来自 `ops_job_last_status`（文件管道任务），
> `$jenkins_job` 来自 `jenkins_build_status`（Jenkins 任务）。未部署 Collector 时 Jenkins 区块显示 No data，属正常。
>
> 表格面板用了 `labelsToFields → merge → organize` 变换链（Prometheus 数据源把标签放在字段元数据里，
> 不先转列就没有 job_name 列）。若列名有出入，只需在面板 Transform 里调整 rename 规则。

## 7. 从 Windows 迁移到 Linux 时

当前 scrape 目标可能是 `192.168.193.1:9105`（Windows）。Linux 部署完成后：

1. 改 Additional Scrape Configs 的 target 为 Linux 虚拟机 IP，保存
2. 在 Prometheus → Targets 确认 `ops-exporter` 变 up
3. Dashboard 的「采集目标」格子自动恢复绿色，"任务总数/状态"即刻有数据（`instance` 标签会变成新地址，无需改面板）

## 8. 验证（Prometheus 查询接口已验证可用）

```bash
# 抓取目标已 up
curl -sk -H "Authorization: Bearer <TOKEN>" \
  "https://rancher.my.org/api/v1/namespaces/cattle-monitoring-system/services/http:rancher-monitoring-prometheus:9090/proxy/api/v1/query?query=up%7Bjob%3D%22ops-exporter%22%7D"

# 逐条验证 promql.md 中的查询（浏览器打开表达式浏览器更直观）：
# ops_job_last_status
# time() - ops_job_last_run_timestamp_seconds
# time() - ops_job_last_success_timestamp_seconds
# time() - ops_exporter_last_refresh_timestamp_seconds
# absent(ops_job_last_status{job_name="database_backup"})

# Jenkins 推送管道（需先部署 Collector 并产生构建，见第 10 节）：
# jenkins_build_status
# jenkins_build_duration_seconds
# sum(increase(jenkins_build_total[24h]))
# sum(increase(jenkins_build_failure_total[24h]))
# 1 - (sum(increase(jenkins_build_failure_total[24h])) / clamp_min(sum(increase(jenkins_build_total[24h])), 1))
# time() - max(jenkins_build_last_timestamp_seconds)
```

**全链路闭环验证清单**（Jenkins 构建 → Grafana 展示）：

1. Jenkins 触发一次构建，等 Collector 的采集间隔（默认配在 config）
2. Exporter 侧确认收到：`curl -s localhost:9105/metrics | grep jenkins_build_`
   - 每个 Job 应出现 5 个序列；`failure_total` 即使从未失败也有 0 序列（不是 No data）
3. Prometheus 侧确认抓到：表达式浏览器查 `jenkins_build_status`，出现 Job 标签
4. Grafana 打开 Dashboard 下半区：对应 `$jenkins_job` 的状态格子变色、耗时/频次图有数据
5. 重复推送同一 build_number：Exporter 返回 `ignored`，`_total` 不增长（幂等去重生效）

## 9. 从源码构建（单一 module，双入口）

仓库是**单一 Go module** `ops-exporter`（无 `go.work`、无子 module）。Exporter 与 Collector 是两个独立入口 `package main`，分别构建出两个二进制；实现分别放在 `internal/exporter` 与 `internal/collector` 两个互不相交的子树，不交叉 import。

```bash
# 根目录下，一次验证覆盖全部包（含两个入口）
go vet ./...
go test ./... -count=2

# 分别构建静态二进制（部署到 Linux）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/ops-exporter-linux-amd64 ./cmd/exporter
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/jenkins-collector-linux-amd64 ./cmd/collector
```

源码布局：

```
cmd/exporter/                    exporter 入口（main）
cmd/collector/                   collector 入口（main）
internal/exporter/config/        配置加载（含 http 超时 + ingest 过期段）
internal/exporter/model/         TaskResult / BuildResult 数据模型与校验
internal/exporter/reader/        JSON 文件读取
internal/exporter/refresher/     周期刷新编排
internal/exporter/metrics/       ops_job_* + jenkins_build_* 指标定义与转换
internal/exporter/store/         BuildStore 内存存储（去重/过期）
internal/exporter/ingest/        POST /api/v1/ingest/build 入口
internal/exporter/expiry/        TTL 过期清理
internal/collector/config/       collector 配置（含 state_file + 环境变量认证）
internal/collector/model/        构建契约模型（与 exporter/model 字段镜像，不共用包）
internal/collector/jenkins/      Jenkins REST API Client
internal/collector/parser/       console log 解析
internal/collector/sender/       HTTP POST + 重试
internal/collector/collector/    ticker 采集编排 + 去重
internal/collector/state/        seen 断点续传（原子落盘）
```

## 10. Jenkins Collector 部署（可选，动态数据接入）

采集器定时从 Jenkins REST API 取最新构建，标准化后 POST 给 Exporter。

**当前目标环境**（已实测连通）：Jenkins 容器化部署在 `192.168.193.147:8081`（Jenkins 2.555.3），仓库内 `config.collector.example.json` 与 `config.collector.docker.json` 的 `jenkins_url` 已指向 `http://192.168.193.147:8081/`。
- 该 Jenkins **强制认证**：匿名访问 `/api/json` 返回 403 `Authentication required`（`/whoAmI/api/json` 匿名可读但无 Job 数据）。因此必须配置 `JENKINS_USERNAME` + `JENKINS_API_TOKEN`（在 Jenkins 用户 → Configure → API Token 生成），否则采集会全部 401/403。
- 同机 `192.168.193.147:9105` 已有 exporter 在跑（可能是旧变体）；部署前需确认它是仓库版本（含 `/api/v1/ingest/build`），否则 Collector 推送会 404。
- `jobs` 字段已按该 Jenkins 上的真实 Job 填写为 `构建任务` / `部署任务`（含中文，Client 用 `url.PathEscape` 编码路径）。

```bash
# 安装二进制与配置
sudo install -m 0755 /tmp/jenkins-collector-linux-amd64 /usr/local/bin/jenkins-collector
sudo mkdir -p /etc/jenkins-collector
sudo cp config.collector.example.json /etc/jenkins-collector/config.json
# 编辑配置：jenkins_url / jobs / exporter_url

# 认证信息推荐放环境变量，不写进配置文件
sudo tee /etc/jenkins-collector/env <<'EOF'
JENKINS_USERNAME=admin
JENKINS_API_TOKEN=<your-api-token>
EOF
```

systemd unit `/etc/systemd/system/jenkins-collector.service`：

```ini
[Unit]
Description=Jenkins Collector
After=network-online.target

[Service]
ExecStart=/usr/local/bin/jenkins-collector -config /etc/jenkins-collector/config.json
EnvironmentFile=/etc/jenkins-collector/env
Restart=on-failure
RestartSec=5
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now jenkins-collector
# Exporter 侧确认数据到达
curl -s localhost:9105/metrics | grep jenkins_build_
```

## 11. Docker Compose 一键部署（推荐，含 Prometheus + Grafana）

仓库根目录已提供完整编排（Task 5）：`docker-compose.yml` + 两套多阶段 Dockerfile +
Prometheus/Grafana provisioning。一套 compose 起全栈，Dashboard 与数据源自动加载。

**文件清单**

| 文件 | 用途 |
|---|---|
| `Dockerfile.exporter` / `Dockerfile.collector` | 多阶段构建（golang builder → alpine），静态二进制；context 均为仓库根目录，分别只编译自己的 `cmd` |
| `docker-compose.yml` | 编排 exporter / collector / prometheus / grafana 四服务 |
| `config.docker.json` | 容器专用 exporter 配置（含 http 超时 + 7 天数据过期） |
| `config.collector.docker.json` | 容器专用 collector 配置（exporter_url 用服务名、state_file 挂卷） |
| `deploy/prometheus/prometheus.yml` | 抓取 `exporter:9105`，15s 间隔 |
| `deploy/grafana/provisioning/` | 数据源（uid=prometheus）+ Dashboard 自动加载 |
| `.env.example` | 认证/密码模板，复制为 `.env` 填真实值 |

**步骤**

```bash
# 1. 填环境变量（认证不进镜像）
cp .env.example .env
#   编辑 .env：JENKINS_USERNAME / JENKINS_API_TOKEN / GRAFANA_ADMIN_PASSWORD

# 2. 编辑 collector 配置填真实 Jenkins 地址和 jobs
#    jenkins_url 已预置为 http://192.168.193.147:8081/；仍需按实际Jenkins上的Job名改 jobs
vi config.collector.docker.json

# 3. 启动监控栈（exporter + prometheus + grafana，不含 collector）
docker compose up -d

# 4.（可选）接入 Jenkins Collector：用 profile 或显式指定服务
docker compose --profile collector up -d

# 5. 验证
curl -s localhost:9105/healthz                    # {"status":"ok"}
curl -s localhost:9105/metrics | grep jenkins_build_   # 推送后可见
# Grafana: http://<host>:3000 → Ops 文件夹 → Ops Exporter Dashboard
# Prometheus: http://<host>:9090 → Status → Targets → ops-exporter UP
```

**关键设计（对应 Task 5 可靠性）**

- **无 Slowloris**：容器配置带 `http.read_header_timeout=5s` 等全套超时
- **数据不过期堆积**：`ingest.max_build_age=168h`（7 天）未更新的 Job 自动清理
  Gauge 序列（Counter 累计值保留）
- **断点续传**：collector 的 `state_file` 挂到 `collector-state` 卷，容器重建后
  不重复推送已处理 Build；认证由环境变量注入，镜像内无密钥
- **Dashboard 单一真源**：直接挂仓库根目录 `grafana-dashboard.json`，避免多副本不同步

> 无 Jenkins 环境时可只起 `docker compose up -d exporter prometheus grafana`，
> Jenkins 区块显示 No data 属正常（未推送构建）。

## 备注

- 二进制为静态编译（CGO_ENABLED=0），无 libc 依赖
- 容器镜像与 compose 见第 11 节（生产推荐）；systemd 裸机部署见第 1~10 节
- 任务方写结果文件时建议原子写入（先写临时文件再 rename），避免 exporter 读到半截 JSON（当前会记文件级错误，不影响其他任务）
