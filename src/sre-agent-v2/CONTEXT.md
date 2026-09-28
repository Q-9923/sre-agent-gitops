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

**Action Attempt**:
根据已批准 Plan 发起的一次受预算约束的修复尝试。

**Verification**:
独立判断修复后目标是否恢复的结果。

## Current Agent Orchestration Boundary

- Agent 在取得 Pod UID 后先执行 `Observe`，再使用 Incident 当前版本申请 `Claim`。
- `INCIDENT_CLAIM_HOLDER_ID` 必须显式配置且不能为空。
- `INCIDENT_CLAIM_LEASE_DURATION` 默认值为 `30s`，并且必须大于零。
- Claim 被其他实例持有、版本冲突或存储访问失败时，Agent 失败关闭，不读取修复状态、不调用模型，也不执行 Kubernetes 动作。
- 模型诊断成功后，Agent 使用 Claim 返回的 Incident 版本作为 fencing token，执行 `DETECTED -> DIAGNOSED` 状态迁移。
- 诊断迁移发生版本冲突或存储错误时，Agent 在策略判断和 Kubernetes 动作之前停止。
- 当前 fencing 只覆盖到诊断状态迁移；动作审批和实际执行前仍需增加第二次 fencing 检查。
- 当前源码尚未部署 PostgreSQL Incident Store，也没有改变 Shadow 环境的 `RESTART_POD_APPROVED=false` 安全边界。
