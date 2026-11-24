# Ops Exporter Linux 部署指南

目标环境：Linux 虚拟机（k3s 集群节点同网段），k3s 集群由 Rancher 管理。
- **Rancher 入口**：`https://rancher.my.org/dashboard/c/local/monitoring`（local 集群 → Monitoring）
- **Prometheus / Grafana 均走 Rancher 代理访问**，不直接暴露独立端口；因此生产上不需要独立 Prometheus/Grafana 容器（见第 11 节）。
- 本会话开发环境无法解析/访问 `rancher.my.org`，以下 Rancher 侧操作均为配置指引，需在能访问 Rancher 的网络内执行。

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

打开 `https://rancher.my.org/dashboard/c/local/monitoring` → 集群 → Monitoring → 定制化
（或 rancher-monitoring 应用 values）→ **Additional Scrape Configs**，
`<LINUX_IP>` 换成运行 exporter 的虚拟机 IP（本环境为 `192.168.193.147`）：

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

从 `https://rancher.my.org/dashboard/c/local/monitoring` → Monitoring → Grafana（走 Rancher 代理，
不直接访问 Grafana 独立端口）→ Dashboards → **New → Import**，
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
> Grafana 通过 Rancher `https://rancher.my.org/dashboard/c/local/monitoring` 代理进入，
> 不要直连 `http://<host>:3000`（本环境无独立 Grafana 端口）。
>
> 两个任务变量互相独立：`$job` 来自 `ops_job_last_status`（文件管道任务），
> `$jenkins_job` 来自 `jenkins_build_status`（Jenkins 任务）。未部署 Collector 时 Jenkins 区块显示 No data，属正常。
>
> 表格面板用了 `labelsToFields → merge → organize` 变换链（Prometheus 数据源把标签放在字段元数据里，
> 不先转列就没有 job_name 列）。若列名有出入，只需在面板 Transform 里调整 rename 规则。
>
> **No data 显示约定（2026-11-24 细调）**：所有 stat 面板的阈值首色统一为中性灰 `#6E7B8F`，
> 配合 `noValue="无数据"`。原因是 Grafana stat 面板无数据时会套用阈值首色，旧配置会让
> 「失败任务数」「距上次成功」显示绿色（误像健康）、「任务状态」显示红色（误像故障）——
> 现在统一为灰色，只表示"未知/无数据"，不携带健康语义。真实值的绿/黄/红阈值与值映射不变。
>
> **「Exporter 新鲜度」**面板：当 `ops_exporter_last_refresh_timestamp_seconds` 为 0（从未刷新）时，
> `time() - 0` 会显示成 `56.9 years` 这类荒谬数值。已加值映射：超过 1 年显示橙色「从未刷新」。
> 出现该提示说明文件管道未工作——通常是 exporter 不是仓库版本或 `result_dir` 下没有合规 JSON，
> 用 `curl -s localhost:9105/metrics | grep ops_exporter_last_refresh` 确认（正常应为近期 Unix 秒）。

## 7. 从 Windows 迁移到 Linux 时

当前 scrape 目标可能是 `192.168.193.1:9105`（Windows）。Linux 部署完成后：

1. 改 Additional Scrape Configs 的 target 为 Linux 虚拟机 IP，保存
2. 在 Prometheus → Targets 确认 `ops-exporter` 变 up
3. Dashboard 的「采集目标」格子自动恢复绿色，"任务总数/状态"即刻有数据（`instance` 标签会变成新地址，无需改面板）

## 8. 验证（Prometheus 查询接口经 Rancher 代理）

**入口**：Rancher `https://rancher.my.org/dashboard/c/local/monitoring`（local 集群 Monitoring 页面）。
Prometheus 与 Grafana 都通过该 Rancher 实例代理访问，不直接暴露 `:9090` / `:3000`。
`<TOKEN>` 为 Rancher API Bearer Token（Rancher 右上角用户 → API & Keys 生成）。

```bash
# 抓取目标已 up（Prometheus 经 Rancher proxy）
curl -sk -H "Authorization: Bearer <TOKEN>" \
  "https://rancher.my.org/api/v1/namespaces/cattle-monitoring-system/services/http:rancher-monitoring-prometheus:9090/proxy/api/v1/query?query=up%7Bjob%3D%22ops-exporter%22%7D"

# Grafana 同样走 Rancher 代理（路径以本集群实际 service 为准，常见形式）：
# https://rancher.my.org/api/v1/namespaces/cattle-monitoring-system/services/http:rancher-monitoring-grafana:80/proxy/
# 也可直接从 Monitoring 页面点 Grafana 链接进入，无需手拼 URL。

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

### 8.1 `jenkins_build_*` 没有数据？按顺序排查

> 现象：`curl -s localhost:9105/metrics | grep jenkins_build_` 没有任何输出。
> 原因几乎总是 **Collector 没在跑 / 认证失败 / 没有已完成构建** 三者之一；
> Exporter 自身只负责接收推送，`/metrics` 里这些序列只有在收到一次成功 POST 后才会出现。

**第 1 步：确认 Collector 容器是否启动**

`collector` 带 profile，默认 `docker compose up -d` **不会**启动它：

```bash
cd /data/jenkins-exporter
docker compose ps                                  # 只看 exporter 属正常
docker compose --profile collector ps              # 这里应看到 jenkins-collector Up
docker compose --profile collector up -d           # 未启动则拉起
```

**第 2 步：看 Collector 日志（最关键，直接给出失败原因）**

```bash
docker compose --profile collector logs --tail=100 collector
```

按日志关键字对号入座：

| 日志关键字 | 原因 | 处理 |
|---|---|---|
| `status 403` / `Authentication required` | Jenkins 认证失败 | 确认 `.env` 里 `JENKINS_USERNAME` / `JENKINS_API_TOKEN` 已填；改完需 `docker compose --profile collector up -d --force-recreate` 重建容器让环境变量生效 |
| `status 404` | Job 名不匹配 | Jenkins 里 Job 名是**区分大小写**的中文名 `构建任务` / `部署任务`；核对 `config.collector.docker.json` 的 `jobs` |
| `get latest build failed` + 网络错误 | 容器无法访问 `192.168.193.147:8081` | 在容器内 `docker compose --profile collector exec collector wget -qO- http://192.168.193.147:8081/whoAmI/api/json` 验证 |
| `build collected ... status=...` | 采集成功 | 回到 Exporter 执行第 3 步验证 |
| 只有 `loaded config`、无任何 `get latest build failed` | 所有 Job 最新构建都未完成（旧版本行为） | 已修复：现查 `lastCompletedBuild`，重新构建/更新镜像后即可采到上一次已完成构建 |

**第 3 步：确认 Exporter 已收到推送**

```bash
curl -s localhost:9105/metrics | grep '^jenkins_build_'
```

- 每个 Job 应有 5 个序列：`status` / `last_timestamp_seconds` / `duration_seconds` / `total` / `failure_total`
- 若日志显示 `build collected` 但这里仍为空：确认 Collector 的 `exporter_url` 指向正在监听 9105 的实例
  （容器内用 `http://exporter:9105/api/v1/ingest/build`；若 Exporter 不是 compose 起的，改用宿主机 IP）

**第 4 步：手动推送一条，隔离 Exporter 与 Collector**

```bash
curl -s -X POST http://127.0.0.1:9105/api/v1/ingest/build \
  -H 'Content-Type: application/json' \
  -d '{"job_name":"手动验证","build_number":1,"status":"success","duration":1.5,"timestamp":1763870400,"failed_stage":""}'
curl -s localhost:9105/metrics | grep '手动验证'
```

- 有输出 → Exporter 正常，问题在 Collector（回到第 2 步）
- 无输出 → 看 Exporter 日志 `docker compose logs --tail=100 exporter`

> 已修复的已知问题：Collector 早期版本只查 `lastBuild`，当 Jenkins 正在执行某个 Job 时
> `result=null`，采集器会跳过，导致截图里「两个 Job 都在构建中」时完全无数据。
> 现在改查 `lastCompletedBuild`，运行中的构建不再阻塞已完成构建的采集。
> 若目标机还是旧二进制/旧镜像，需重新构建并重启 Collector 才能生效。

### 8.2 Exporter 重启后 Gauge 面板 No data（但 Counter 有数据）

**症状**：`构建总数(24h)` / `失败构建数(24h)` / 趋势图有数值，但 `构建状态` / `最近耗时` /
`构建成功率` / `最近构建距今` 全 No data。

**根因**：Exporter 的 BuildStore 是内存存储，重启即清空；而 Collector 的断点 `state.json`
记着"这些 build 已处理过"，不会再发 → `jenkins_build_status` / `duration` /
`last_timestamp` 这些 Gauge 永远回不来（Counter 因 Prometheus 侧历史样本仍在，
`increase()` 仍有数值，所以看起来"一半有数据一半没有"）。

**已修复：心跳重发**。`config.collector.docker.json` 的 `resend_interval` 默认 `2m`：
每隔该周期，Collector 会把每个 Job 缓存的最新 Build 重发一次。
- Exporter 正常时：相同 build_number 被幂等拒绝（返回 `ignored`），**不会重复计数**；
- Exporter 重启后：内存为空，重发被接受，Gauge 在 ≤2 分钟内自动恢复。

日志关键字：`heartbeat resent job=... build_number=...`。设 `"resend_interval": "0s"` 可禁用
（退回旧行为）。注意：心跳只重发"曾经成功发送过"的 Build，不制造新数据。Collector 重启后
即使内存缓存丢失也会从 Jenkins 现场重建 Build 并立即补发一次，因此 **Exporter 与 Collector
同时重启也能恢复**。

> 生效前提：目标机需同步最新 Collector 代码并重建镜像；旧镜像没有心跳逻辑。

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

## 11. Docker Compose 部署（业务容器 + 复用 Rancher 监控栈）

**目标环境已确认**：k3s 集群由 Rancher 管理，Rancher Monitoring **已内置 Prometheus + Grafana**。
因此 `docker-compose.yml` **只编排 exporter 与可选 collector 两个服务**，不含 Prometheus/Grafana
（已彻底移除，避免与 Rancher 内置栈重复）。监控栈一律走 Rancher：

- `docker compose up -d` 只起 exporter → Rancher 按第 5 节配置抓取
  `192.168.193.147:9105` → 按第 6 节把 Dashboard 导入 Rancher Grafana（数据源选内置 Prometheus）。
- 需要采集 Jenkins 时：`docker compose --profile collector up -d`。

仓库根目录提供 `docker-compose.yml` + 两套多阶段 Dockerfile。Rancher 侧的抓取配置与 Dashboard
导入步骤见第 5、6 节；`deploy/prometheus/` 与 `deploy/grafana/provisioning/` 目录保留作参考样例，
但不被 compose 使用。

**文件清单**

| 文件 | 用途 |
|---|---|
| `Dockerfile.exporter` / `Dockerfile.collector` | 多阶段构建（golang builder → alpine），静态二进制；context 均为仓库根目录，分别只编译自己的 `cmd`；用 `GOFLAGS=-mod=vendor` 离线构建 |
| `vendor/` | 由 `go mod vendor` 生成的依赖快照（约 12M），随构建 context 传入，构建不访问 Go 模块代理 |
| `docker-compose.yml` | 只编排 exporter（默认）+ collector（profile `collector`）；**无 prometheus/grafana** |
| `config.docker.json` | 容器专用 exporter 配置（含 http 超时 + 7 天数据过期）；`result_dir=/data/log`，对应 compose 的宿主目录绑定挂载 |
| `config.collector.docker.json` | 容器专用 collector 配置（exporter_url 用服务名、state_file 挂卷） |
| `deploy/prometheus/prometheus.yml` | 独立部署参考样例（compose 不引用）；生产用 Rancher 抓宿主 IP，见第 5 节 |
| `deploy/grafana/provisioning/` | 独立部署参考样例（compose 不引用）；生产用 Rancher Grafana 导入，见第 6 节 |
| `.env.example` | 认证模板，复制为 `.env` 填 JENKINS_USERNAME / JENKINS_API_TOKEN |

**步骤**

```bash
# 1. 填环境变量（认证不进镜像）
cp .env.example .env
#   编辑 .env：JENKINS_USERNAME=admin / JENKINS_API_TOKEN=<token>

# 2. 编辑 collector 配置
#    jenkins_url 已预置 http://192.168.193.147:8081/，jobs 已填 构建任务/部署任务
vi config.collector.docker.json

# 3. 启动 exporter（compose 仅此服务 + 可选 collector；监控栈走 Rancher）
docker compose up -d

# 4.（可选）接入 Jenkins Collector（需 .env 凭据）
docker compose --profile collector up -d

# 5. 验证 exporter
curl -s localhost:9105/healthz                         # {"status":"ok"}
curl -s localhost:9105/metrics | grep jenkins_build_   # 推送后可见

# 6. 验证监控栈（Rancher 内置，按第 5、6 节已配好）
#    Rancher Grafana → Ops 文件夹 → Ops Exporter Dashboard（数据源选内置 Prometheus）
#    Rancher Prometheus → Status → Targets → ops-exporter UP（target 192.168.193.147:9105）
```

**文件管道数据源：宿主机 `/data/log`**

`config.docker.json` 的 `result_dir` 已设为 `/data/log`，compose 用**只读绑定挂载**
`- /data/log:/data/log:ro` 把宿主机目录映射进容器（原先的 `exporter-data` named volume 已删除）。
Exporter 每隔 `refresh_interval` 扫描该目录下所有 `.json` 文件，按 `job_name` 产出 `ops_job_*`。
数据格式与旧变体写出的 `database_backup` / `file_sync` / `broken_task` 等命名兼容，无需改名。

若目标机 9105 当前跑的是旧变体镜像，需同步仓库最新代码（**含 `vendor/`**）并重建 exporter 才生效：

```bash
cd /data/jenkins-exporter
ls /data/log/*.json                                  # 先确认目录里有数据文件
docker compose up -d --build exporter                # 重建为仓库版本并挂载 /data/log
docker compose restart exporter                      # 确保新 config 与挂载生效
curl -s localhost:9105/metrics | grep '^ops_job_'    # 应看到各 job 的 status/duration/last_run
curl -s localhost:9105/metrics | grep '^ops_exporter_last_refresh'   # 应为接近当前的 Unix 秒
```

`ops_exporter_last_refresh_timestamp_seconds` 若为 `0`，说明目录读取失败，
用 `docker compose logs exporter | grep 'refresh failed'` 查看具体路径错误。

**关键设计（对应 Task 5 可靠性）**

- **离线构建（重要）**：依赖已 `go mod vendor` 固化到 `vendor/`，两个 Dockerfile 设 `GOFLAGS=-mod=vendor`，构建全程不访问 `proxy.golang.org`。内网/CI 无外网时原 `RUN go mod download` 会失败（实测报错 `exit code: 1`），现改为直接 COPY vendor 编译。**注意：必须把整个仓库（含 `vendor/`）传到构建机**，不能用 `.dockerignore` 排除它。升级依赖后需重跑 `go mod vendor` 同步快照。
- **无 Slowloris**：容器配置带 `http.read_header_timeout=5s` 等全套超时
- **数据不过期堆积**：`ingest.max_build_age=168h`（7 天）未更新的 Job 自动清理
  Gauge 序列（Counter 累计值保留）
- **断点续传**：collector 的 `state_file` 挂到 `collector-state` 卷，容器重建后
  不重复推送已处理 Build；认证由环境变量注入，镜像内无密钥
- **Dashboard 单一真源**：仓库根目录 `grafana-dashboard.json` 是唯一版本，按第 6 节导入 Rancher Grafana
- **复用 Rancher 监控栈**：compose 只含 exporter + 可选 collector，不含 prometheus/grafana；抓取配置与 Dashboard 全部走 Rancher（第 5、6 节）

> **抓取目标核验提醒**：当前开发沙箱对 `192.168.193.147:9105` 的请求经本地代理转发（返回 502 / `via 127.0.0.1`），因此在开发机上 curl 该地址的结果**不能代表目标机真实状态**。grab 是否 UP 请以 Rancher Prometheus → Status → Targets，或在 192.168.193.147 本机上 `curl localhost:9105/healthz` 为准。

> 无 Jenkins 环境时可只起 `docker compose up -d`（exporter），
> Jenkins 区块显示 No data 属正常（未推送构建）。

## 备注

- 二进制为静态编译（CGO_ENABLED=0），无 libc 依赖
- 容器镜像与 compose 见第 11 节（生产推荐）；systemd 裸机部署见第 1~10 节
- 任务方写结果文件时建议原子写入（先写临时文件再 rename），避免 exporter 读到半截 JSON（当前会记文件级错误，不影响其他任务）
