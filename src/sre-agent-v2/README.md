# SRE Agent v2

SRE Agent v2 是一个面向 Kubernetes 的安全建议与受控修复 Agent。当前源码基线使用 Go
`1.26.5`、Kubernetes client-go `v0.32.13`、pgx `v5.10.0` 和 PostgreSQL `17`
语义。

## Verification Worker

Verification Worker 将已经存在的领域接缝串成持续、可恢复的闭环：

```text
terminal Action Attempt
  -> Fenced Verification Begin
  -> PENDING Verification
  -> ClaimPendingVerification
  -> bounded Kubernetes verification
  -> CompleteFencedVerification
  -> RECOVERED only: Resolve
```

每次循环优先处理已持久化的 `RECOVERED` Resolution 工作，因此 Resolve 失败或 Agent
重启不会重新执行 Kubernetes Verification。`NOT_RECOVERED` 与 `INCONCLUSIVE` 会持久化为
终态，但不会将 Incident 迁移到 `RESOLVED`。

Worker 的八个外部失败点均向上返回错误，并由运行循环记录稳定的结构化错误码。连续失败采用
以 `POLL_INTERVAL` 为基数、最大 `8x` 的指数退避；一次成功迭代后退避重置。上下文取消会
立即停止，正常取消不记录为失败。

## Kubernetes 恢复证据

当前源码验证器只支持稳定 `ReplicaSet` Subject。它要求：

- Subject 集群、名称和 UID 仍匹配当前 ReplicaSet；
- `status.observedGeneration` 已观察到当前 generation；
- `spec.replicas` 为正，Ready/Available 状态满足期望副本数；
- 存在足量由该 ReplicaSet 控制、非 terminating、Ready 且不是原动作目标 UID 的替代 Pod。

ReplicaSet GET、Pod List、NotFound、UID 改变、非法 selector、client 未配置和上下文取消都返回
错误并失败关闭，不能降级为 `NOT_RECOVERED`。

## 关键配置

| 环境变量 | 当前默认/Shadow 值 | 约束 |
| --- | --- | --- |
| `POLL_INTERVAL` | `30s` | 必须大于零；同时是 Worker 基础重试间隔 |
| `KUBERNETES_REQUEST_TIMEOUT` | `15s` | 每次独立 Verification 的硬超时 |
| `INCIDENT_CLAIM_HOLDER_ID` | Pod UID | 必填且非空白；每个 Agent 实例唯一 |
| `INCIDENT_CLAIM_LEASE_DURATION` | `30s` | 必须严格大于 `KUBERNETES_REQUEST_TIMEOUT` |
| `INCIDENT_STORE_BACKEND` | `memory` | 仅允许 `memory` 或 `postgres` |
| `INCIDENT_STORE_POSTGRES_DSN` | 未配置 | 仅在 `postgres` 后端必填，必须来自 Secret |
| `INCIDENT_STORE_CONNECT_TIMEOUT` | `10s` | 必须大于零 |
| `INCIDENT_STORE_MIGRATION_TIMEOUT` | `30s` | 必须大于零 |

`apps/sre-agent-v2/deployment.yaml` 当前仍显式使用单副本 Memory Shadow。生产 PostgreSQL 服务、
Secret、持久卷、备份/恢复和 5432 NetworkPolicy 尚未确定前，不得把 live 清单切到 `postgres`
或扩为多副本。

## 本地验证

```bash
gofmt -w .
go mod tidy
go test ./... -count=1
go test -race ./... -count=1
go test -tags=integration ./... -count=1
go test -race -tags=integration ./... -count=1
go vet ./...
go build ./...
git diff --check
```

Integration 与 integration race 需要可用的 Docker/Testcontainers。迁移必须保持
`001` 到 `014` 连续且只追加。

运行、故障演练与 PostgreSQL 上线门槛见
[`docs/verification-worker-runbook.md`](docs/verification-worker-runbook.md)，领域语言见
[`CONTEXT.md`](CONTEXT.md)。

## 当前边界

源码与单元测试通过不等于生产闭环已经上线。只有构建不可变镜像、更新 digest、完成 PostgreSQL
基础设施和 NetworkPolicy、通过真实双副本竞争/Lease takeover/重启恢复/依赖故障演练并验证回滚
后，才能把 Verification Worker 标记为生产就绪，也才能把项目事实批准进入 RAG 的生产语料。
