# Incident Management

该上下文负责把重复告警观察归并为可恢复、可审批、可审计的故障生命周期。

## Language

**Observation**:
告警源在某一时刻提供的一次规范化故障观察；重复 Observation 不等于新的 Incident。
_Avoid_: Event, Incident

**Incident**:
一个目标在一次连续故障期间的持久生命周期；终结后再次发生的故障属于新的 Incident。
_Avoid_: Alert, Observation

**Active Incident**:
尚未进入终结状态、仍代表当前故障的一条 Incident。

**Idempotency Key**:
用于把同一目标、同一故障类型的重复 Observation 归并到同一 Active Incident 的稳定身份。

**Transition**:
Incident 从一个合法状态进入另一个合法状态的领域变化。

**Claim**:
Lease Holder 对某个 Active Incident 获得的限时排他推进权；Claim 只协调处理权，不代表 Approval 或修复授权。
_Avoid_: Approval, Remediation

**Lease**:
Claim 保持有效的期限；Lease 到期后，其他 Lease Holder 可以重新竞争该 Incident 的 Claim。
_Avoid_: Cooldown, Approval Expiry

**Lease Holder**:
当前持有 Incident Claim、可以尝试推进其生命周期的 Agent 实例。
_Avoid_: Approver, Actor

**Fencing Token**:
每次 Claim 成功后递增的单调代次；持有旧代次的 Lease Holder 不得继续推进 Incident。
_Avoid_: Credential, Secret

**Claim Audit Event**:
每次成功 Claim 产生的不可变审计记录，包含 Incident 版本、Lease Holder、取得时间和到期时间，并按 Incident 版本排序。
_Avoid_: Active Claim, Approval

**Plan**:
针对特定 Incident 和目标资源生成的候选修复方案。

**Approval**:
对指定 Incident、Plan 和目标资源身份授予的有限期执行许可。

**Action Key**:
由 Incident、Plan Hash 和目标 UID 组成的稳定动作身份；Lease Holder 或 Fencing Token 变化不会产生新的 Action Key。
_Avoid_: Retry Key, Request ID

**Execution Key**:
由 Action Key 和 Fencing Token 组成的单次执行所有权身份；它用于 fencing，不作为跨 Claim 代次的幂等身份。
_Avoid_: Action Key, Credential

**Action Attempt**:
根据已批准 Plan 发起的一次受预算约束的修复尝试；同一 Action Key 只能对应一个 Action Attempt，其最初的 Fencing Token 用于标识发起该尝试的所有权代次。

**Action Attempt Status**:
Action Attempt 的持久化执行结果：

- `STARTED`：已取得执行资格，但尚未持久化确定结果。
- `SUCCEEDED`：Kubernetes 动作调用已成功返回。
- `FAILED`：Kubernetes 动作调用返回了确定的失败结果。
- `UNKNOWN`：旧执行器失联后无法安全判断动作结果；不得自动重放，必须进入独立 Verification。

`SUCCEEDED`、`FAILED` 和 `UNKNOWN` 均为终态；同一 Action Key 不得创建或执行第二个 Action Attempt。
_Avoid_: Incident State, Verification

**Verification**:
独立于 Action Attempt 结果、针对同一动作执行的有界恢复检查；不得根据 Kubernetes 请求结果推断目标已经恢复。
_Avoid_: Action Result

**Agent Verification Orchestration**:
Action Attempt 进入 `SUCCEEDED`、`FAILED` 或 `UNKNOWN` 终态后，Agent 必须通过 Fenced Verification Begin，在同一个原子持久化边界内校验 Incident 当前版本、Claim Holder 和 Lease，将对应 Incident 持久迁移或确认在 `VERIFYING`，并为同一 Action Key 创建或复用唯一的 `PENDING` Verification。任一 fencing 校验、状态迁移、审计或 Verification 写入失败时必须整体失败或回滚；不得重放 Kubernetes 动作，也不得提前迁移到 `RESOLVED`。

**Agent Verification Scheduling Seam**:
Agent 对已持久化 `PENDING` Verification 的发现与限时派发边界；调度成功会为其所属的 `VERIFYING` Incident 建立新的 Claim，使同一 Lease 期间仅一个 Lease Holder 获得处理权，Lease 到期后可以重新竞争。本接缝不执行 Verification，也不产生 Verification Result。
_Avoid_: Verification Execution, Verification Result

**Agent Verification Execution Seam**:
Agent 只能对已持久化的 `PENDING` Verification，基于其稳定 Verification Subject 运行独立、有界的恢复检查。检查结果只能是 `RECOVERED`、`NOT_RECOVERED` 或 `INCONCLUSIVE`，并必须连同非空 Evidence Code 通过乐观版本校验持久化。执行器不可用、检查失败、结果非法或结果持久化失败时必须失败关闭并保持原有 `PENDING` 结果；已终态的 Verification 不得再次执行。本接缝不负责发现待执行 Verification，也不得直接将 Incident 迁移到 `RESOLVED`。

**Fenced Verification Complete**:
Agent 只能在 Incident 仍处于 `VERIFYING`，且 Incident 版本、Claim Holder、未过期 Lease 与已持久化 Claim Audit 全部匹配时，将对应 `PENDING` Verification 原子持久化为 `RECOVERED`、`NOT_RECOVERED` 或 `INCONCLUSIVE`。相同参数的精确重试必须返回同一终态结果，不得再次推进 Incident 或 Verification 版本；旧 Holder、过期 Lease、版本冲突、缺失 Claim Audit 或 Verification 写入失败必须失败关闭且不得改变已有状态。本接缝不直接将 Incident 迁移到 `RESOLVED`。
_Avoid_: Unfenced Verification Result, Incident Resolution

**Resolution Evidence**:
指向已持久化且状态为 `RECOVERED` 的 Verification；只有其 Action Key 归属当前 Incident 时，才能结束 Verifying Incident。
_Avoid_: Action Attempt Result, Evidence Code

**Verification Subject**:
Verification 所观察的稳定工作负载身份，包含集群、命名空间、资源类型、名称和 UID；它与 Action Key 中被执行动作的原始目标 UID 不同，替代 Pod 的 UID 变化不得改变 Verification Subject。
_Avoid_: Action Target, Evidence

**Verifying Incident**:
正在针对稳定 Verification Subject 进行独立恢复验证的活跃 Incident；Action Attempt 的执行结果不能代替其恢复结论。
_Avoid_: Resolved Incident, Action Attempt Status

**Verification Status**:
Verification 的生命周期状态；`PENDING` 表示等待独立证据，`RECOVERED`、`NOT_RECOVERED` 和 `INCONCLUSIVE` 是不可变终态。
_Avoid_: Action Attempt Status

**Waiting Approval**:
Incident 已形成确定的候选 Plan，但尚未取得与该 Plan 和目标资源身份完全匹配的有效 Approval；该 Incident 仍保持活跃并可在后续恢复处理。
_Avoid_: Handled, Resolved

**Approval Binding**:
Incident 等待审批时保存的 Plan 身份与目标资源身份组合，用于确保后续 Approval 只能授权同一个候选方案和同一个资源实例。
_Avoid_: Approval, Active Claim
