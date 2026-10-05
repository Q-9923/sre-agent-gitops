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
