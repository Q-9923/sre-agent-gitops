# sre-agent-gitops

SRE Agent 的源码、Kubernetes 清单和 Argo CD GitOps 配置仓库。

## SRE Agent v2

SRE Agent v2 周期性读取 Prometheus 告警，收集 Kubernetes 上下文，调用本地 Ollama 模型生成候选决策，再由确定性的策略、审批和执行边界决定是否允许修复操作。

当前 Shadow 模式默认设置：

- 只处理允许命名空间中的受支持告警；
- 模型只能生成候选决策，不能绕过策略层直接操作 Kubernetes；
- `RESTART_POD_APPROVED=false` 时禁止执行 Pod 重启；
- 使用事件去重、冷却时间、执行次数限制和结构化日志；
- Kubernetes ServiceAccount 使用最小权限。

源码位于 `src/sre-agent-v2/`，部署清单位于 `apps/sre-agent-v2/`。

## Operational HTTP 接口

Agent 在 `HEALTH_ADDRESS` 上提供统一的 operational HTTP 服务，默认监听 `:8080`。

| 路径 | 正常状态 | 含义 |
| --- | --- | --- |
| `/livez` | HTTP 200 | Agent 的处理周期仍在推进，没有超过 `LIVENESS_STALE_AFTER` |
| `/readyz` | HTTP 200 | 最近一次成功处理周期没有超过 `READINESS_STALE_AFTER` |
| `/metrics` | HTTP 200 | Prometheus 文本格式的运行指标 |

`/livez` 和 `/readyz` 语义相互独立：

- 外部依赖暂时失败时，readiness 可以变为 503；
- 处理周期长期不再推进时，liveness 变为 503，由 Kubernetes liveness probe 触发重启；
- Agent 启动后，在第一次成功周期完成前，readiness 返回 503。

## Prometheus Metrics

`/metrics` 使用独立的 Prometheus Registry，当前暴露：

- Go Runtime Metrics，例如 `go_goroutines`；
- SRE Agent 业务指标：

```text
sre_agent_cycles_total{result="<RESULT>"}

```

允许的 `result` 标签值：

- `NO_ACTION`
- `PROCESSED`
- `ERROR`
- `UNKNOWN`

未知结果统一归一化为 `UNKNOWN`，避免 incident ID、Pod 名称、错误文本等动态内容形成高基数标签。

应用关闭导致的 `context.Canceled` 不记录为 `ERROR`。

## Prometheus 告警规则

`apps/sre-agent-v2/prometheusrule.yaml` 当前定义两个 `warning` 级别告警：

- `SREAgentMetricsTargetDown`：采集目标连续 2 分钟返回 `up == 0`，或从服务发现中消失；
- `SREAgentCycleErrorsIncreasing`：最近 5 分钟新增至少 3 个失败周期，并持续 2 分钟。

规则由 Prometheus 负责计算，通知是否发送以及发送到哪个管理员渠道，由现有 Alertmanager 路由和 Receiver 配置决定；本清单本身不定义通知接收人。

这两个告警名称不属于 SRE Agent 当前支持的修复告警，不会触发 Pod 重启或其他自动修复动作。

规则测试使用与集群一致的 Prometheus 版本：

```bash
apps/sre-agent-v2/scripts/test-prometheus-rules.sh
```

## 本地验证

```bash
cd src/sre-agent-v2

go mod verify
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build -o bin/sre-agent .
```

启动 Agent 后可以检查：

```bash
curl -i http://127.0.0.1:8080/livez
curl -i http://127.0.0.1:8080/readyz
curl -fsS http://127.0.0.1:8080/metrics
```

只有 Agent 正在对应地址运行时，上述 `curl` 才能成功。

集群级持续采集已通过 `apps/sre-agent-v2/service.yaml` 和 `apps/sre-agent-v2/servicemonitor.yaml` 接入 Prometheus Operator。ServiceMonitor 由 `release=prometheus-stack` 选择器发现，采集目标 `sre-agent-v2-metrics` 已验证为 `up=1`；可使用 `sre_agent_cycles_total` 查询 Agent 周期处理结果。
