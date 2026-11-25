# Ops Exporter 告警配置指南

> 基线：2026-11-25  
> 适用范围：Rancher 内置 Prometheus / Grafana / Alertmanager。  
> 当前方案：**不使用 alertai，不单独部署 Prometheus/Grafana/Alertmanager，不创建 AlertmanagerConfig CR。**

---

## 0. 先说结论

这套告警只需要做三件事：

1. 把 recording rules 写入 Helm values 的 `prometheus.additionalPrometheusRulesMap`；
2. 把 alerting rules 写入 Helm values 的 `prometheus.additionalPrometheusRulesMap`；
3. 把原生 Alertmanager 配置写入 Helm values 的 **`alertmanager.config`**（不是 `additionalConfigString`）。

> **2026-11-25 关键修正：** 之前把原生配置放进了
> `alertmanager.alertmanagerSpec.additionalConfigString`，chart 会把它原样塞进
> `Alertmanager` CR 的 `spec`，导致升级时出现
> `unknown field "spec.global" / "spec.receivers" / "spec.route"` 警告，
> 而且 Alertmanager 实际不会使用这份配置。现在已全部改为 `alertmanager.config`。

已经生成这些合并文件：

| 文件 | 用途 |
|---|---|
| `deploy/rancher-monitoring-values.yaml` | **邮件版 Helm values**，推荐用于 `helm upgrade -f` |
| `deploy/rancher-monitoring-values-webhook.yaml` | **Webhook 版 Helm values**，个人项目 / 无邮箱链路测试用 |
| `deploy/alertmanager/alertmanager.yml` | 原生 Alertmanager 配置（邮件版） |
| `deploy/alertmanager/alertmanager-webhook.yml` | 原生 Alertmanager 配置（Webhook 版），可用于直接 patch Secret |

Helm values 包含：

- `prometheus.additionalPrometheusRulesMap.ops-recording`
- `prometheus.additionalPrometheusRulesMap.ops-alerting`
- `alertmanager.config`（原生 global / route / receivers / inhibit_rules）
- `alertmanager.secret.recreateIfExists: true`（覆盖 Rancher 默认不会更新的 Alertmanager Secret）
- `alertmanager.alertmanagerSpec.additionalConfigString: ""`（清空旧的错误配置）

优先使用 Helm values 文件；仓库里的源文件保留作为可读版本。修改时各文件要同步。

**不要进入这个页面：**

`Monitoring → Alerting → AlertmanagerConfig → Create`

那个页面创建的是 Kubernetes CRD 对象，不是原生 Alertmanager 配置文件。截图里的错误：

```text
Cannot read properties of undefined (reading 'namespace')
```

就是因为把原生 `alertmanager.yml` 粘进了 AlertmanagerConfig CR 创建页。原生配置以 `global:` 开头，没有 `apiVersion / kind / metadata / spec`，Rancher 前端读取 `metadata.namespace` 时直接报 JavaScript 错误。

### 0.1 直接编辑 Rancher App YAML 时的关键层级

从现场导出的 Rancher Monitoring App 配置可以确认，当前环境是：

```text
chart: rancher-monitoring 110.0.2+up80.9.1-rancher.22
namespace: cattle-monitoring-system
release: rancher-monitoring
```

如果你不是从 UI 的 Customize 表单录入，而是直接编辑 App 的 YAML，所有用户自定义 values 必须放在 **`spec.values`** 下面。

你当前原始文件里的这两段内容：

- `prometheus.additionalPrometheusRulesMap`
- `alertmanager.alertmanagerSpec.additionalConfigString`

其中第一段应保留，第二段已改为正确的 `alertmanager.config`。它们原来被写在了根级别（`status` 之后）：

```yaml
apiVersion: catalog.cattle.io/v1
kind: App
metadata: ...
spec: ...
status: ...

prometheus:            # 错：不在 spec.values 下
  ...
alertmanager:          # 错：不在 spec.values 下
  ...
```

根级别的 `prometheus` / `alertmanager` 不属于 `App` CRD 的 schema，通常会被 API Server 裁剪或忽略，结果是**配置看起来写了，但根本没有生效**。

正确结构是：

```yaml
apiVersion: catalog.cattle.io/v1
kind: App
metadata:
  name: rancher-monitoring
  namespace: cattle-monitoring-system
spec:
  chart:
    metadata:
      name: rancher-monitoring
    version: 110.0.2+up80.9.1-rancher.22
  version: 2
  values:                         # 必须在这里
    prometheus:
      additionalPrometheusRulesMap:
        ops-recording:
          groups: ...             # 内容见 recording_rules.yml
        ops-alerting:
          groups: ...             # 内容见 alerting_rules.yml
    alertmanager:
      secret:
        recreateIfExists: true
      config:                         # 原生 Alertmanager 配置放这里
        global: ...
        route: ...
        receivers: ...
        inhibit_rules: ...
      alertmanagerSpec:
        additionalConfigString: ""    # 必须清空旧值
```

> 通过 Rancher UI 的 Customize → Advanced 录入时，不需要自己写 `spec.values` 这一层，UI 会负责保存。只有在直接编辑 App YAML 时才需要关心。

---

## 1. 告警链路

```text
Ops Exporter / Jenkins Collector
        │
        │ /metrics
        ▼
Rancher 内置 Prometheus
        │
        ├─ recording_rules.yml：预计算，统一 Grafana 与告警口径
        │
        ├─ alerting_rules.yml：10 条告警规则，生成 ALERTS
        ▼
Rancher 内置 Alertmanager
        │
        ├─ 分组：同一任务同类告警合并
        ├─ 路由：critical / warning / info 不同节奏
        ├─ 抑制：上游停摆时静默失真告警
        ▼
email 主路径 / webhook 可选路径
```

三个文件的职责不能混：

| 文件 | 内容类型 | 录入位置 | 作用 |
|---|---|---|---|
| `deploy/prometheus/rules/recording_rules.yml` | Prometheus recording rules | Additional Recording Rules | 预计算表达式，供面板和告警共用 |
| `deploy/prometheus/rules/alerting_rules.yml` | Prometheus alerting rules | Additional Alerting Rules | 定义 10 条告警什么时候触发 |
| `deploy/alertmanager/alertmanager.yml` | 原生 Alertmanager 配置 | Helm values `alertmanager.config` | 定义告警怎么分组、路由、抑制、通知 |

---

## 2. 配置步骤

### 2.1 录入记录规则

Rancher 中进入集群，然后：

```text
Monitoring / 监控
  → 编辑/升级 Rancher Monitoring
  → Customize / 自定义选项
  → Advanced / 高级
  → Additional Recording Rules
```

把 `deploy/prometheus/rules/recording_rules.yml` 的内容完整贴入。

验证方式：Prometheus UI 的 `Status → Rules` 里能看到规则组：

```text
ops-exporter-recording
```

也可以在 Prometheus 表达式浏览器查：

promql

```promql
ops_exporter:rules_loaded:bool
```

有值即说明记录规则已经加载。

> 如果 Rancher 版本只有一个 Additional Rules 输入框，把 `recording_rules.yml` 和 `alerting_rules.yml` 里的 `groups:` 列表合并成一个 YAML 贴入即可，不要丢失任意一个 group。

---

### 2.2 录入告警规则

同样在：

```text
Monitoring → 编辑/升级 → Customize → Advanced → Additional Alerting Rules
```

把 `deploy/prometheus/rules/alerting_rules.yml` 完整贴入。

验证方式：Prometheus UI 的 `Status → Rules` 里能看到：

```text
ops-exporter-alerts
```

告警触发时，表达式浏览器可以查到：

promql

```promql
ALERTS{alertstate="firing"}
```

---

### 2.3 录入 Alertmanager 配置

**当前推荐方式：Helm values 的 `alertmanager.config`。**

不要再用 `alertmanager.alertmanagerSpec.additionalConfigString`，原因见第 0 节。
已生成可直接使用的文件：

```text
deploy/rancher-monitoring-values.yaml          # 邮件版
deploy/rancher-monitoring-values-webhook.yaml  # Webhook 版
```

如果确实要在 Rancher UI 里手工录入，则进入
`Monitoring → 编辑/升级 → Customize → Advanced → Alertmanager Config`，
把下面内容放到 `alertmanager.config` 下，而不是 `additionalConfigString`。
仓库源文件 `deploy/alertmanager/alertmanager.yml` 以可读形式保留，优先以合并后的 values 文件为准。

```yaml
global:
  resolve_timeout: 5m
  # 按实际 SMTP 修改；无企业邮箱时可用 Mailpit 测试，见第 5 节
  smtp_smarthost: 'smtp.example.com:25'
  smtp_from: 'alertmanager@example.com'
  smtp_require_tls: false
  # 需要认证时：
  # smtp_auth_username: 'alertmanager@example.com'
  # smtp_auth_password_file: /etc/alertmanager/secrets/smtp-password

inhibit_rules:
  # Exporter 刷新停摆后，文件管道任务告警都不可信。
  # 显式排除源告警本身，避免依赖 Alertmanager 的双向匹配特殊处理。
  - source_matchers:
      - alertname="OpsExporterStale"
    target_matchers:
      - service="ops-exporter"
      - alertname!="OpsExporterStale"
    equal: ["service"]

  # Jenkins Collector 停摆后，具体 Job 的构建告警不可信。
  - source_matchers:
      - alertname="JenkinsBuildStale"
    target_matchers:
      - service="jenkins"
      - alertname!="JenkinsBuildStale"
    equal: ["service"]

route:
  group_by: ["alertname", "service", "job_name"]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  receiver: email-ops

  routes:
    - receiver: email-ops
      matchers:
        - severity="critical"
      group_wait: 10s
      group_interval: 1m
      repeat_interval: 1h
      continue: false

    - receiver: email-ops
      matchers:
        - severity="warning"
      group_wait: 30s
      group_interval: 5m
      repeat_interval: 4h
      continue: false

    - receiver: email-ops
      matchers:
        - severity="info"
      group_wait: 1m
      group_interval: 15m
      repeat_interval: 12h
      continue: false

receivers:
  - name: email-ops
    email_configs:
      - to: 'ops@example.com'
        send_resolved: true
        # 不要写自定义 Subject 模板，原因见下方说明。

  # 可选 webhook 路径，见第 5.2 节。启用时把上面所有 receiver 改成 webhook-ops。
  # - name: webhook-ops
  #   webhook_configs:
  #     - url: 'http://<TARGET_HOST>:9999/webhook'
  #       send_resolved: true
```

> **重要：Alertmanager 配置必须放在 `alertmanager.config`。**  
> 放在 `alertmanager.alertmanagerSpec.additionalConfigString` 会导致两个问题：
>
> 1. chart 把内容塞进 `Alertmanager` CR 的 `spec`，产生
>    `unknown field "spec.global" / "spec.receivers" / "spec.route"` 警告；
> 2. 若配置里带 Alertmanager 模板（如 `{{ .Status | toUpper }}`），
>    chart 还会做一次 Helm `tpl` 渲染，直接升级失败：
>
> ```text
> Error: UPGRADE FAILED: ... function "toUpper" not defined
> ```
>
> 现在 `alertmanager.config` 在 `tplConfig: false`（默认值）下只做 `toYaml`，
> 不会二次渲染。个人项目建议直接用 Alertmanager 默认邮件 Subject，最稳。

保存后等待 Rancher Monitoring Operator 同步，一般 1 到 2 分钟。检查 Alertmanager UI 的 `Status` 页面是否显示配置版本正常、没有解析错误。

---

## 3. 十条告警分别是什么意思

| 告警 | 级别 | 持续时间 | 触发条件 | 处理方向 |
|---|---|---:|---|---|
| `OpsExporterStale` | critical | 1m | `ops_exporter:last_refresh_age:seconds > 60` | Exporter 刷新线程停摆，检查进程、`/data/log` 挂载、目录权限 |
| `OpsJobStale` | warning | 10m | `job:ops_job_last_run_age:seconds > 86400` | 任务超过 24h 没执行，检查调度器和结果文件 |
| `OpsJobFailed` | warning | 5m | `job:ops_job_status:value == 0` | 最近一次任务失败，检查任务日志 |
| `OpsJobNeverSucceeded` | info | 30m | `ops_job_last_success_timestamp_seconds == 0` | 任务从未成功过；`0` 是合法状态，不是缺数据 |
| `OpsJobAbsentDatabaseBackup` | warning | 10m | `absent(...{job_name="database_backup"})` | 关键任务没有产生任何指标，检查是否接入 Exporter |
| `OpsJobAbsentFileSync` | warning | 10m | `absent(...{job_name="file_sync"})` | 同上，检查 `file_sync` 接入 |
| `JenkinsBuildFailed` | warning | 5m | `jenkins_build_status == 0` | 最近一次构建非成功，包含 failed/unstable/aborted |
| `JenkinsFailureRateHigh` | warning | 15m | `job:jenkins_build_failure_rate:24h > 0.1` | 24h 失败率超过 10% |
| `JenkinsBuildStale` | info | 10m | `jenkins:build_last_age:seconds > 86400` | Collector 可能停摆，或 Jenkins 长时间无构建 |
| `JenkinsBuildDurationSpike` | warning | 5m | `jenkins_build_duration_seconds > 2 * job:jenkins_build_duration_avg:7d` | 构建耗时超过 7 天均值 2 倍 |

注意：

- `OpsJobNeverSucceeded` 用的是 `last_success == 0`，这是合法告警条件；
- `absent()` 返回的序列没有标签，所以 `database_backup`、`file_sync` 必须逐条写明任务名；
- Jenkins 告警依赖 Collector 推送构建数据，没有 `jenkins_build_*` 指标时这些规则不会触发。

---

## 4. 为什么这样路由和抑制

### 4.1 分级通知节奏

| 级别 | 首次等待 | 同组新告警间隔 | 未恢复重复间隔 | 用途 |
|---|---:|---:|---:|---|
| critical | 10s | 1m | 1h | 链路级故障，需要尽快知道 |
| warning | 30s | 5m | 4h | 任务/构建异常，标准节奏 |
| info | 1m | 15m | 12h | 长期问题，减少打扰 |
| default | 30s | 5m | 4h | 未命中上面三条的兜底 |

`group_by: ["alertname", "service", "job_name"]` 的含义是：同一个任务上的同类告警合并成一封通知，避免每个标签组合都单独发邮件。

### 4.2 抑制规则

```text
OpsExporterStale 触发
  → 静默除它自己以外的 service=ops-exporter 告警

JenkinsBuildStale 触发
  → 静默除它自己以外的 service=jenkins 告警
```

原因：上游数据源已经不可信时，下游任务告警只是连锁反应。保留根因告警，静默次要告警，避免告警风暴。

配置里显式写了 `alertname!="OpsExporterStale"` 和 `alertname!="JenkinsBuildStale"`，这样源告警不会落到自己的 target 匹配里，逻辑更清楚。

---

## 5. 没有企业邮箱时怎么测试

不要为了测试另起一套 Alertmanager。仍然用 Rancher 内置的 Alertmanager，只替换通知目标。

### 5.1 方案 A：Mailpit 本地 SMTP

在一台 Alertmanager 能访问到的 Linux 主机上执行：

```bash
docker run -d --name mailpit \
  -p 1025:1025 \
  -p 8025:8025 \
  axllent/mailpit
```

修改 `alertmanager.yml`：

```yaml
global:
  resolve_timeout: 5m
  smtp_smarthost: '<TARGET_HOST>:1025'
  smtp_from: 'alertmanager@example.com'
  smtp_require_tls: false
```

收件人保持任意地址即可：

```yaml
receivers:
  - name: email-ops
    email_configs:
      - to: 'ops@example.com'
        send_resolved: true
```

浏览器打开：

```text
http://<TARGET_HOST>:8025
```

如果 Rancher Alertmanager 跑在 Kubernetes 内部，`<TARGET_HOST>:1025` 必须从集群节点可访问；必要时用 NodePort、Service 或集群内部署 Mailpit。

---

### 5.2 方案 B：Webhook 测试接收器

先启动一个最小 HTTP 服务：

```bash
cat > /tmp/alert-webhook.py <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(n).decode('utf-8', 'replace')
        print(body, flush=True)
        self.send_response(200)
        self.end_headers()

    def log_message(self, fmt, *args):
        pass

HTTPServer(('0.0.0.0', 9999), Handler).serve_forever()
PY

python3 /tmp/alert-webhook.py
```

在 `alertmanager.yml` 中启用 webhook receiver：

```yaml
  - name: webhook-ops
    webhook_configs:
      - url: 'http://<TARGET_HOST>:9999/webhook'
        send_resolved: true
```

然后把 `route.receiver` 和三条子路由的 `receiver` 全部从 `email-ops` 改成 `webhook-ops`。

二选一即可：邮件和 webhook 不要同时作为默认 receiver，否则会重复通知，也不利于排查。

---

## 6. 验证清单

按顺序检查，不要跳步：

1. Prometheus `Status → Rules` 中存在：
   - `ops-exporter-recording`
   - `ops-exporter-alerts`

2. Prometheus 表达式浏览器执行：

   promql

   ```promql
   ops_exporter:rules_loaded:bool
   ```

   有值。

3. 表达式浏览器执行：

   promql

   ```promql
   ALERTS{alertstate="firing"}
   ```

   能查到测试触发的告警。

4. Alertmanager UI 的 `Alerts` 页面能看到同一条告警。

5. Alertmanager UI 的 `Status` 页面没有配置解析错误。

6. 邮件或 webhook 收到 `FIRING` 通知。

7. 告警恢复后收到 `RESOLVED` 通知，因为 receiver 配了 `send_resolved: true`。

8. 若直接编辑过 App YAML，确认 values 真的保存在 `spec.values` 下：

   ```bash
   kubectl -n cattle-monitoring-system get app rancher-monitoring -o yaml \
     | grep -nE 'additionalPrometheusRulesMap|alertmanager:|config:'
   ```

   如果查不到，说明配置被写到了错误层级或已被裁剪。

9. 确认 Helm release 中不再有错误的 `additionalConfigString`：

   ```bash
   helm -n cattle-monitoring-system get values rancher-monitoring \
     | grep -nE 'additionalConfigString|recreateIfExists|ops-recording'
   ```

   `additionalConfigString` 应为空字符串；其余两项应能看到。

10. 直接检查 Alertmanager 实际使用的 Secret：

    ```bash
    kubectl -n cattle-monitoring-system get secret \
      alertmanager-rancher-monitoring-alertmanager \
      -o jsonpath='{.data.alertmanager\.yaml}' | base64 -d | head -40
    ```

    输出应是我们的 `global / route / receivers`，不是 chart 默认的 `null` receiver。

---

## 7. 常见错误

| 现象 | 原因 | 处理 |
|---|---|---|
| `Cannot read properties of undefined (reading 'namespace')` | 在 AlertmanagerConfig CR 创建页粘贴了原生 `alertmanager.yml` | 退出该页面，改用 Helm values 的 `alertmanager.config` |
| App YAML 里写了规则但 Prometheus 看不到 | `prometheus` / `alertmanager` 写在了根级别，不在 `spec.values` 下 | 移到 `spec.values` 下，或用 Rancher UI 的 Customize 表单重新录入 |
| 升级时 `unknown field "spec.global"` / `"spec.receivers"` / `"spec.route"` / `"spec.inhibit_rules"` | 原生 Alertmanager 配置误放进 `alertmanager.alertmanagerSpec.additionalConfigString`，被塞进 Alertmanager CR 的 `spec` | 改用 `alertmanager.config`，并在 values 里显式写 `additionalConfigString: ""` 清空旧值 |
| Helm 升级成功但 Alertmanager 配置没变 | Rancher 默认不会覆盖已存在的 Alertmanager Config Secret | values 中保留 `alertmanager.secret.recreateIfExists: true`，或直接 patch Secret（见第 5.2 节） |
| Prometheus Rules 看不到规则组 | 规则没录入，或 Rancher rule selector 不匹配 | 用 Additional Rules 方式录入，避免 selector 问题 |
| `ops_exporter:rules_loaded:bool` 不存在 | 记录规则未加载 | 检查 `recording_rules.yml` 是否语法正确并已保存 |
| Alertmanager 看不到告警 | Prometheus 没把告警发给 Alertmanager，或规则标签不匹配 | 先在 Prometheus `ALERTS` 确认告警存在，再查 Alertmanager 配置和路由 |
| 告警触发但收不到邮件 | SMTP 地址、发件人、收件人是占位符；或认证/TLS 配置错误 | 按实际环境替换；测试阶段用 Mailpit |
| 收到很多重复邮件 | `group_by` 太细或 `repeat_interval` 太短 | 保持默认分组，调大对应级别的 `repeat_interval` |
| 根因告警也被抑制 | source 和 target matcher 重叠 | 使用当前配置里的 `alertname!="..."` 排除源告警 |
| `从未成功` 告警没触发 | 误以为 `last_success=0` 是缺数据 | `0` 是合法值，规则本身正确 |
| `关键任务缺失` 告警没触发 | 用错了 `absent()` 写法 | 必须写 `absent(metric{job_name="具体任务名"})` |

---

## 8. 修改原则

- 改阈值或查询口径：先改 `promql.md`，再同步 `recording_rules.yml` 和 `alerting_rules.yml`；
- 改收件人、SMTP、通知节奏、抑制关系：只改 `alertmanager.yml`；
- 不要同时维护 AlertmanagerConfig CR 和原生配置；
- 不要在仓库里写真实密码。SMTP 密码用 Secret 或 `_file` 方式引用；
- 每改一次配置，至少验证一次 `FIRING` 和一次 `RESOLVED`。

---

## 9. Helm 方式（不走 Rancher UI）

如果不想处理 Rancher App CR，可以直接用 Helm 管理。

### 9.1 确认已有 Helm release

```bash
helm list -n cattle-monitoring-system
```

应该能看到 `rancher-monitoring`。先备份当前 values：

```bash
helm -n cattle-monitoring-system get values rancher-monitoring \
  > helm-values.backup.yaml
```

### 9.2 添加仓库并升级

```bash
helm repo add rancher-charts https://charts.rancher.io
helm repo update

helm upgrade rancher-monitoring rancher-charts/rancher-monitoring \
  --namespace cattle-monitoring-system \
  --version 110.0.2+up80.9.1-rancher.22 \
  --reuse-values \
  -f deploy/rancher-monitoring-values.yaml \
  --wait --timeout 10m
```

`--reuse-values` 的作用是保留原有的 retention、storage、Grafana 等自定义值，只用我们的文件覆盖规则与 Alertmanager 配置。

如果是全新安装：

```bash
helm install rancher-monitoring rancher-charts/rancher-monitoring \
  --namespace cattle-monitoring-system \
  --create-namespace \
  --version 110.0.2+up80.9.1-rancher.22 \
  -f deploy/rancher-monitoring-values.yaml \
  --wait --timeout 10m
```

### 9.3 验证 Helm values

```bash
helm -n cattle-monitoring-system get values rancher-monitoring \
  | grep -nE 'additionalPrometheusRulesMap|ops-recording|recreateIfExists|additionalConfigString'
```

应能看到规则和 `recreateIfExists: true`，且 `additionalConfigString: ""`。随后仍按第 6 节在 Prometheus / Alertmanager UI 中验证规则和通知。

> 若之前已经用错误 values 升级过，必须显式带 `additionalConfigString: ""` 再升级一次，
> `--reuse-values` 才会把旧值清掉，警告也会随之消失。

### 9.4 注意 Rancher App CR 的冲突

如果 Rancher 里的 `App/rancher-monitoring` 仍然存在，它的控制器可能把 Helm values 覆盖回去。

- 临时方案：先直接 `helm upgrade`，观察是否稳定；
- 彻底方案：先在 Rancher 删除该 App（会卸载监控），再用 `helm install` 重新安装；
- `rancher-monitoring-crd` 不要动，CRD 由它单独管理。

---

## 10. Webhook 版本（个人项目 / 无企业邮箱）

已经生成 webhook 版 Helm values：

```text
deploy/rancher-monitoring-values-webhook.yaml
```

与邮件版的区别只有通知出口：

- 主路由和三条子路由的 `receiver` 全部改为 `webhook-ops`；
- 新增 `webhook-ops` receiver；
- `email-ops` receiver 保留但不参与路由，方便随时切回邮件。

默认 URL 是本地测试地址，必须替换：

```text
http://<TARGET_HOST>:9999/webhook
```

### 10.1 本地测试接收器

在 Alertmanager 能访问到的 Linux 主机上执行：

```bash
cat > /tmp/alert-webhook.py <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(n).decode('utf-8', 'replace')
        print(body, flush=True)
        self.send_response(200)
        self.end_headers()

    def log_message(self, fmt, *args):
        pass

HTTPServer(('0.0.0.0', 9999), Handler).serve_forever()
PY

python3 /tmp/alert-webhook.py
```

然后触发一条测试告警，终端会打印 Alertmanager 发来的 JSON，包含 `status`、`labels`、`annotations` 等字段。这一步能验证：

```text
Prometheus 规则 → Alertmanager 路由 → webhook 通知
```

整条链路是否打通。

### 10.2 用 webhook values 升级

```bash
helm upgrade rancher-monitoring rancher-charts/rancher-monitoring \
  --namespace cattle-monitoring-system \
  --version 110.0.2+up80.9.1-rancher.22 \
  --reuse-values \
  -f deploy/rancher-monitoring-values-webhook.yaml \
  --wait --timeout 10m
```

切回邮件版时，把 `-f` 换回 `deploy/rancher-monitoring-values.yaml` 即可。

### 10.3 webhook 没打印 JSON 的排查顺序

本地接收器启动后没有输出，按这个顺序查，不要只盯着 Jenkins 是否有失败构建：

1. **确认 Alertmanager 实际用的是 webhook 配置，而不是旧邮件配置。**

   ```bash
   kubectl -n cattle-monitoring-system get secret \
     alertmanager-rancher-monitoring-alertmanager \
     -o jsonpath='{.data.alertmanager\.yaml}' | base64 -d \
     | grep -nE 'receiver:|webhook_configs|email_configs'
   ```

   必须看到 `receiver: webhook-ops` 和 `webhook_configs`。如果还是 `email-ops`，
   说明 Helm values 没有真正写入 Secret，回到第 0 节重新升级。

2. **确认接收器在监听，且 Alertmanager Pod 能访问到。**

   ```bash
   ss -lntp | grep 9999
   kubectl -n cattle-monitoring-system run curltest --rm -it --restart=Never \
     --image=curlimages/curl -- curl -sS -XPOST \
     http://<TARGET_HOST>:9999/webhook -d '{"test":"direct"}'
   ```

   第二条命令执行后，本地终端应立即打印 `{"test":"direct"}`。

3. **绕过 Prometheus，直接给 Alertmanager 发一条测试告警。**

   ```bash
   AM_POD=$(kubectl -n cattle-monitoring-system get pod \
     -l app.kubernetes.io/name=alertmanager -o name | head -1)
   kubectl -n cattle-monitoring-system port-forward "$AM_POD" 19093:9093 &

   curl -i -XPOST http://127.0.0.1:19093/api/v2/alerts \
     -H 'Content-Type: application/json' \
     -d '[{"labels":{"alertname":"TestWebhook","severity":"critical","service":"ops-exporter"},"annotations":{"summary":"webhook link test"}}]'
   ```

   `curl -i` 要能看到 `HTTP/1.1 202 Accepted`。用 `19093` 是为了避开本机可能已存在的
   9093 监听；若报 `address already in use`，先执行
   `ss -lntp | grep 9093` 查占用者，`kill` 掉旧的 port-forward 即可。

   critical 路由 `group_wait: 10s`，等待 10 到 20 秒再看本地终端。
   这一步通了，说明 Alertmanager → webhook 没问题，剩下的只可能是 Prometheus 没产生告警。

4. **确认 Prometheus 侧真的有 firing 告警。**

   ```bash
   kubectl -n cattle-monitoring-system port-forward svc/prometheus-operated 9090:9090 &
   # 浏览器打开 http://127.0.0.1:9090/alerts
   ```

   或在表达式浏览器查：

   ```promql
   ALERTS{alertstate="firing"}
   jenkins_build_status
   ```

   Jenkins 页面显示 FAILURE 不等于 Prometheus 已产生告警：
   - `JenkinsBuildFailed` 要求 `jenkins_build_status == 0` 持续 5 分钟；
   - 该指标来自 Collector 推送的构建数据，Collector 未接入时这个序列根本不存在；
   - warning 路由还有 30 秒 `group_wait`。

5. **仍无输出时看 Alertmanager 日志。**

   ```bash
   kubectl -n cattle-monitoring-system logs -l app.kubernetes.io/name=alertmanager \
     --tail=200 | grep -iE 'notify|webhook|error|failed'
   ```

### 10.4 如果最终仍要发到个人邮箱

个人邮箱可以用 SMTP，但要注意：

- 用**授权码 / App Password**，不是邮箱登录密码；
- 以 QQ 邮箱为例：

```yaml
global:
  smtp_smarthost: 'smtp.qq.com:587'
  smtp_from: 'yourname@qq.com'
  smtp_require_tls: true
  smtp_auth_username: 'yourname@qq.com'
  # 个人项目可临时内联；正式环境建议用 Secret 或 _file 引用
  smtp_auth_password: 'YOUR_APP_PASSWORD'
```

替换后在邮件版 values 的 `alertmanager.config.global` 中同步修改，再用邮件版文件升级。不要同时让 webhook 和 email 都作为默认 receiver，否则会重复通知。
