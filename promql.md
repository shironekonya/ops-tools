# Ops Exporter 告警查询集（PromQL）

对应 TODO 7。第一版不在 exporter 内实现告警引擎，
以下查询供 Prometheus 告警规则（`alerting rules`）和 Grafana Dashboard（TODO 12）直接使用。

## 1. 任务超时未执行（核心查询）

```promql
time() - ops_job_last_run_timestamp_seconds > 86400
```

- `ops_job_last_run_timestamp_seconds` 是任务最近一次**结束**时间的 Unix 秒数
- 阈值按任务周期调整：日备任务用 86400（1 天），小时级任务改 7200 等
- 配合 `for: 10m` 防抖，避免刷新周期内的瞬时波动触发告警

**为什么任务文件消失后依然有效**：exporter 对"曾经出现过、后来文件没了"的任务，
保留序列最后一次的值不清理（`TestStaleSeriesPersistAfterFileRemoval` 锁定了该行为）。
文件停止更新后，`time() - last_run` 会随时间持续增长，告警照常触发。

## 2. exporter 自身失效（前置保障）

```promql
time() - ops_exporter_last_refresh_timestamp_seconds > 60
```

- 若 exporter 卡死、任务目录持续读不了，所有第 1 类查询会同时失真
- 该指标只在整轮刷新成功时更新，阈值取刷新间隔（默认 10s）的数倍即可
- **阈值应与配置联动**：配置文件里 `refresh_interval` 改成多少，本条阈值就应设为
  它的 3~6 倍（如 10s 间隔配 60s 阈值，30s 间隔配 180s）。写在告警规则注释里，
  改配置时同步检查
- 建议优先级高于任何单任务告警：它失效意味着所有任务数据都不可信

## 3. 任务执行失败

```promql
ops_job_last_status == 0
```

- 1 = 最近一次成功，0 = 最近一次失败
- 注意这是"最近一次"的状态：连续失败不会累积，需要趋势时用 `avg_over_time` 补充

## 4. 任务从未成功过

```promql
ops_job_last_success_timestamp_seconds == 0
```

- exporter 明确约定：从未成功过的任务该值为 0（失败不会覆盖它，也不会留空）
- `0` 是合法的告警条件，不是缺数据

## 5. 任务从 exporter 中消失 / 从未执行过

任务如果**从未**产生过结果文件，exporter 一无所知，不会为它伪造任何序列
（`TestUnknownJobNeverFaked` 锁定了该行为）。这类任务必须用 `absent()` 盯住：

```promql
absent(ops_job_last_run_timestamp_seconds{job_name="database_backup"})
```

- 每个关键任务写一条，任务清单来自运维配置，而不是 exporter 自动发现
- 与第 1 条互补：第 1 条管"跑过但停了"，本条管"根本没跑过"

## 6. 组合示例：完整告警规则片段

```yaml
groups:
  - name: ops-exporter
    rules:
      - alert: OpsJobStale
        expr: time() - ops_job_last_run_timestamp_seconds > 86400
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "任务 {{ $labels.job_name }} 超过 24 小时未执行"

      - alert: OpsJobFailed
        expr: ops_job_last_status == 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "任务 {{ $labels.job_name }} 最近一次执行失败"

      - alert: OpsJobNeverSucceeded
        expr: ops_job_last_success_timestamp_seconds == 0
        for: 30m
        labels:
          severity: info
        annotations:
          summary: "任务 {{ $labels.job_name }} 从未成功过"

      - alert: OpsExporterStale
        expr: time() - ops_exporter_last_refresh_timestamp_seconds > 60
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Ops Exporter 超过 60 秒未成功刷新任务结果"
```

---

## 二、Jenkins 构建管道（推送来源，`jenkins_build_*`）

以上第 1~6 节针对**文件管道**（`ops_job_*`）。以下查询针对 **Jenkins 推送管道**，
数据链路：Jenkins REST API → jenkins-collector → `POST /api/v1/ingest/build` → Exporter。
两组指标注册在同一 registry、同一个 Prometheus job（`ops-exporter`），一次抓取全拿到。

### 指标契约（标签只有 `job_name`）

| 指标 | 类型 | 语义 |
|---|---|---|
| `jenkins_build_status` | Gauge | 最近一次构建：1=success，0=failed/unstable/aborted |
| `jenkins_build_last_timestamp_seconds` | Gauge | 最近一次构建**结束**时刻（Unix 秒） |
| `jenkins_build_duration_seconds` | Gauge | 最近一次构建耗时（秒） |
| `jenkins_build_total` | Counter | 累计收到构建数（每次被接受 +1），重启归零，`rate/increase` 已内建处理 |
| `jenkins_build_failure_total` | Counter | 累计失败数（仅 status=failed +1；unstable/aborted 不计失败） |

注意：Counter 在 exporter 重启后从 0 重新计数，这不是缺陷——Prometheus 的
`rate()`/`increase()` 对计数器重置有内建容忍，告警一律基于 `rate/increase`，不要直接用累计值做绝对值判断。

### 7. 最近一次构建失败

```promql
jenkins_build_status == 0
```

- 与 `ops_job_last_status == 0` 风格一致：1=成功，0=非成功。
- 这是"最近一次"状态；要看连续失败用第 9 条失败率。

### 8. 构建失败数（窗口内）

```promql
sum by(job_name) (increase(jenkins_build_failure_total[24h]))
```

- 用 `increase` 而非裸 Counter，自动处理 exporter 重启归零。
- 配合 `or (count(...) > 0) * 0`：完全无数据时保留 No data、有序列但无失败时显示 0（Grafana 面板同款防假绿写法）。

### 9. 构建失败率（窗口内）

```promql
1 - (
  sum by(job_name) (increase(jenkins_build_failure_total[24h]))
  /
  clamp_min(sum by(job_name) (increase(jenkins_build_total[24h])), 1)
)
```

- `clamp_min(..., 1)` 防除零；窗口内零构建时分母为 1，失败率为 0。
- Dashboard「构建成功率（24h）」即 `1 - 本式` 的全局聚合版。

### 10. 构建长时间未完成 / Collector 停摆

```promql
time() - max(jenkins_build_last_timestamp_seconds) > 86400
```

- 取所有 Jenkins Job 中**最近一次**构建结束时刻距今秒数。持续增大说明
  Collector 停摆、Jenkins 长时间无构建、或推送链路中断。
- 阈值按 Jenkins 流水线周期调整（日构建用 86400，小时级用 7200）。
- 与文件管道的 `OpsExporterStale` 互补：本条盯"构建数据新鲜度"，那条盯"exporter 刷新线程"。

### 11. 构建耗时突增（相对基线）

```promql
jenkins_build_duration_seconds
  > 2 * avg_over_time(jenkins_build_duration_seconds[7d])
```

- `jenkins_build_duration_seconds` 是每次构建的最新值（阶梯状），配合 7 天均值做相对阈值，
  比绝对阈值更能适应不同 Job 的耗时量级。

### 12. 组合示例：Jenkins 告警规则片段

```yaml
groups:
  - name: jenkins-build
    rules:
      - alert: JenkinsBuildFailed
        expr: jenkins_build_status == 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Jenkins 任务 {{ $labels.job_name }} 最近一次构建非成功"

      - alert: JenkinsFailureRateHigh
        expr: |
          1 - (
            sum by(job_name) (increase(jenkins_build_failure_total[24h]))
            / clamp_min(sum by(job_name) (increase(jenkins_build_total[24h])), 1)
          ) > 0.1
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Jenkins 任务 {{ $labels.job_name }} 24h 失败率超过 10%"

      - alert: JenkinsBuildStale
        expr: time() - max(jenkins_build_last_timestamp_seconds) > 86400
        for: 10m
        labels:
          severity: info
        annotations:
          summary: "Jenkins 构建数据超过 24h 未更新（Collector 可能停摆）"
```

> 阈值（5m/15m 防抖、10% 失败率、24h 新鲜度）与 `grafana-dashboard.json`
> 的 Jenkins 区块同源，改一处请同步另一处。
> `unstable`/`aborted` 不计入 `failure_total`（语义上不是"构建失败"），但会让
> `jenkins_build_status` 显示 0——如需把 unstable 也算失败，改 `buildupdater.go` 的计数分支。
