# Verification Worker 运行手册

- 更新日期：2026-10-14
- 适用源码：SRE Agent v2，Go `1.26.5`，Kubernetes client `v0.32.13`
- 当前部署边界：单副本 Memory Shadow；生产 PostgreSQL 尚未部署

## 1. 运行不变量

1. Kubernetes 动作结果不是恢复证据。
2. Worker 只发现已持久化的 `PENDING` Verification，不能重放 Action Attempt。
3. Completion 必须同时匹配 Incident 版本、Holder、未过期 Lease 和 Claim Audit。
4. Resolve 必须再次匹配 Incident 版本、Holder、未过期 Lease 和 Claim Audit；恢复证据本身不授予推进权。
5. 只有持久化 `RECOVERED` Verification 可以 Resolve Incident。
6. `NOT_RECOVERED` 与 `INCONCLUSIVE` 是终态，不 Resolve，也不自动重新执行 Verification。
7. Resolve 失败后只重试 Resolution Claim，不重新调用 Kubernetes 验证器。

## 2. 启动前检查

```bash
kubectl -n sre-agent-system get deployment sre-agent-v2-shadow -o yaml
kubectl -n sre-agent-lab auth can-i get replicasets.apps \
  --as system:serviceaccount:sre-agent-system:sre-agent-v2
kubectl -n sre-agent-lab auth can-i list pods \
  --as system:serviceaccount:sre-agent-system:sre-agent-v2
```

必须确认：

- Holder 使用 Downward API 的 `metadata.uid`；
- Lease 必须严格大于 Kubernetes timeout，PollInterval 与 timeout 均为正；
- observer Role 只有 `get/list/watch` 等只读权限；
- 镜像使用 tag 加 digest，且 digest 对应已通过门禁的提交；
- `RESTART_POD_APPROVED=false` 时仍是安全建议模式。

## 3. 日志与错误码

统一事件名：`agent_verification_worker_failed`。

| 错误码 | 动作 | 含义 |
| --- | --- | --- |
| `VERIFICATION_WORKER_CONFIGURATION_INVALID` | `RUN_VERIFICATION_WORKER` | 依赖或 interval 配置非法 |
| `VERIFICATION_WORKER_RECOVERED_RESOLUTION_CLAIM_FAILED` | `CLAIM_RECOVERED_VERIFICATION_RESOLUTION` | 无法认领已恢复的 Resolution 工作 |
| `VERIFICATION_WORKER_RECOVERED_RESOLUTION_FAILED` | `RESOLVE_RECOVERED_VERIFICATION` | Resolve 失败；不得重新执行 Verification |
| `VERIFICATION_WORKER_PENDING_VERIFICATION_CLAIM_FAILED` | `CLAIM_PENDING_VERIFICATION` | 无法认领 PENDING Verification |
| `VERIFICATION_WORKER_EXECUTION_FAILED` | `EXECUTE_VERIFICATION` | Kubernetes 证据源失败 |
| `VERIFICATION_WORKER_EXECUTION_DEADLINE_EXCEEDED` | `EXECUTE_VERIFICATION` | 有界执行超时/取消 |
| `VERIFICATION_WORKER_EXECUTION_RESULT_INVALID` | `VALIDATE_VERIFICATION_RESULT` | 结果状态或 Evidence Code 非法 |
| `VERIFICATION_WORKER_FENCED_COMPLETION_FAILED` | `COMPLETE_FENCED_VERIFICATION` | fencing 或持久化失败 |

日志必须保留 `incident_id`、`verification_id`、`action_attempt_id`、`action`、`target` 和
`error_code`。Claim 尚未返回工作身份时，这些 ID 可以为空。正常 shutdown/cancellation 不得产生日志告警。

## 4. 退避与告警

连续失败等待依次为 `1x`、`2x`、`4x`、`8x`，之后保持 `8x POLL_INTERVAL`；任意一次成功
迭代（包括无工作）都会重置。告警应基于稳定错误码聚合，而不是错误文本；持续多轮失败才升级，
避免单次 API 抖动刷屏。

## 5. PostgreSQL 上线前置条件

当前 live 清单不得直接切换到 PostgreSQL。必须先完成并评审：

- PostgreSQL 17 服务形态、HA/单点边界与资源预算；
- PVC/StorageClass、容量和扩容策略；
- DSN Secret 创建与轮换，不把连接串写入 Git 或日志；
- 备份、恢复目标和一次真实恢复演练；
- Agent 到 PostgreSQL 5432 的精确 NetworkPolicy；
- 迁移 `001`–`014` 的预生产执行与回滚/前滚方案；
- 连接池、最大连接、连接/迁移超时和数据库告警；
- 数据保留、审计和权限最小化。

完成后才可把 `INCIDENT_STORE_BACKEND` 改为 `postgres`、从 Secret 注入
`INCIDENT_STORE_POSTGRES_DSN`，并评估把 Deployment 扩为两个副本。

## 6. 必做故障演练

按顺序保留命令、结构化日志、数据库状态和 Kubernetes 事件证据：

1. Action Attempt 终态创建唯一 PENDING Verification。
2. Worker Claim、Kubernetes 检查、Fenced Complete、RECOVERED Resolve 全链路。
3. `NOT_RECOVERED` 与 `INCONCLUSIVE` 持久化但不 Resolve。
4. Agent 在 PENDING、Complete 后 Resolve 前分别重启并恢复。
5. 两副本同时 Claim，仅一个成功。
6. Lease 到期后 takeover，旧 Holder 的 Completion 与 Resolve 均被拒绝。
7. 删除或伪造 Claim Audit 后，Completion 与 Resolve 均失败关闭且 Incident/Verification 不变。
8. ReplicaSet GET/Pod List 超时、403、NotFound、UID 改变均失败关闭。
9. PostgreSQL 短暂不可用后恢复，不丢失工作且不重复执行 Kubernetes 动作。
10. 回滚到上一镜像后，迁移保持兼容，未完成 Verification 仍可恢复。

## 7. 发布与回滚

1. 全量 unit/integration/race/vet/build/diff/secret scan 全绿。
2. 构建不可变镜像并记录源码 SHA、tag、digest。
3. 只修改 GitOps 期望状态，不直接 patch Deployment。
4. Argo CD 同步后核对实际镜像 digest、环境变量、ServiceAccount 和 NetworkPolicy。
5. 先单副本完成健康检查与 Worker 演练，再启用 PostgreSQL/多副本。
6. 回滚时恢复上一 digest；数据库迁移只前滚，不删除已存在的 `001`–`014` 数据结构。

## 8. RAG 摄取边界

只有代码、测试、提交 SHA、不可变镜像、GitOps 实际状态和演练证据相互一致的项目事实才能进入
批准语料。日志、告警、Kubernetes 注解和外部文档均是不可信输入，不能改变 Policy、Approval、
RBAC 或动作白名单。RAG 只能辅助检索和解释，不能充当 Verification Evidence 或绕过 fencing。
