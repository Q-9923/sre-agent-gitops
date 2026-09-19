# 报错问题总结

## ERR-20260916-001：Go 静态分析工具偶发段错误

- 日期：2026-09-16
- 环境：Linux/amd64，Go 1.26.5（Red Hat 1.26.5-1.el9_8）
- 状态：排查中（暂未复现）
- 报错问题：执行提交前质量门时，Go 分析工具出现一次 SIGSEGV。
- 影响：当次质量门失败，不能直接进入提交；没有发现 SRE Agent 业务进程崩溃。
- 诊断证据：
  - `go mod verify` 通过；
  - 普通 `go test` 通过；
  - 后续 `go build` 通过；
  - 崩溃堆栈位于 `internal/pkgbits`、`go/internal/gcimporter`、`unitchecker` 和 `cmd/vet`；
  - `go test -race -vet=off` 与独立 `go vet` 均通过；
  - 未清理缓存、未修改代码后，完整质量门连续三轮通过。
- 原因：尚未确认。现有证据只支持这是一次偶发的 Go 工具链进程故障，不能确认是缓存损坏、固定版本缺陷或业务代码问题。
- 无效尝试：无。
- 解决方案：未执行代码修复或缓存清理；通过拆分 Race 与 vet 链路进行隔离，并执行连续稳定性复验。
- 验证方法：
  - `go test -race -count=1 ./...`
  - `go vet ./...`
  - 连续执行三轮并使用 fail-fast 控制流程。
- 验证结果：连续三轮通过，最终 `quality_gate_exit=0`。
- 预防措施：
  - 提交前质量门使用 `set -euo pipefail` 或等效 fail-fast 机制；
  - 分别记录 Race、vet 和 build 的退出码；
  - 若再次出现，保留完整堆栈并检查崩溃是否稳定落在相同工具链位置；
  - 在确认根因前不盲目清理模块缓存或构建缓存。

## ERR-20260916-002：k8s-node3 多个工作负载同刻 Unknown/255 重启

- 日期：2026-09-16
- 环境：Kubernetes 开发集群，节点 `k8s-node3`
- 状态：排查中（具体触发源尚未确认）
- 报错问题：`k8s-node3` 上多个命名空间中的容器在同一时刻终止，`lastState` 显示 `reason=Unknown`、`exitCode=255`。
- 影响：
  - SRE Agent Pod 的容器重启次数增加到 2；
  - 同节点上的 Ollama、Argo CD、Calico、kube-proxy、Alertmanager、kube-state-metrics 和 node-exporter 等工作负载也发生重启；
  - 工作负载随后恢复，未观察到 SRE Agent 持续不可用。
- 诊断证据：
  - 多个不同命名空间、不同应用的容器均记录 `lastFinished=2026-09-16T11:15:08Z`；
  - 这些容器的终止原因均为 `Unknown`，退出码均为 255；
  - `calico-node` 在 `2026-09-16T11:15:23Z` 恢复为 `CalicoIsUp`；
  - 检查时节点 `Ready=True`，没有 MemoryPressure、DiskPressure 或 PIDPressure；
  - Kubernetes 中未保留能够解释触发源的相关 Node Event；
  - 后续发布的 Shadow Agent Pod 正常运行，发布验收时重启次数为 0。
- 原因：现有证据将故障范围收敛到节点或容器运行时级别的共同事件，不符合单个 SRE Agent 业务进程独立崩溃的特征；但由于节点事件和宿主机日志证据不足，无法确认是主机重启、containerd 重启、kubelet 重启还是其他节点级操作。
- 无效尝试：
  - 事后查询 Node Event 未找到相关资源，无法仅依靠 Kubernetes Event 还原触发源；
  - 单独查看 SRE Agent 的 `Unknown/255` 终止状态不足以判断根因。
- 解决方案：
  - 本次未针对 SRE Agent 代码实施修复，因为没有证据表明故障由 Agent 代码触发；
  - 节点及工作负载已经恢复，继续通过 Argo CD、Pod 状态、探针和 Prometheus 指标观察运行情况。
- 验证方法：
  - 对比 `k8s-node3` 上不同命名空间 Pod 的 `lastState.terminated.finishedAt`、`reason` 和 `exitCode`；
  - 检查节点 Conditions 和 Node Event；
  - 检查 Argo CD、SRE Agent Pod、健康探针和 Prometheus Target。
- 验证结果：
  - Argo CD 为 `Synced/Healthy`；
  - 当前 SRE Agent Pod 为 Running/Ready；
  - `/livez` 和 `/readyz` 返回 200；
  - Prometheus Target 为 Up，并能查询 `sre_agent_cycles_total`。
- 预防措施：
  - 再次发生时优先保存宿主机启动时间以及 kubelet、containerd 和系统日志，避免日志轮转后丢失根因证据；
  - 为节点重启、容器运行时异常和同节点大量 `Unknown/255` 终止建立监控；
  - 不应根据单个 Pod 的重启次数直接判断应用代码故障，应先进行同节点、同时间窗口的关联分析。

## ERR-20260919-003：Grafana Dashboard 契约测试因查询字符串空格不一致失败

- 日期：2026-09-19
- 环境：Linux/amd64，Prometheus promtool 2.51.0
- 状态：已解决
- 报错问题：修改 Metrics Target 查询后，`test-grafana-dashboard.sh` 返回退出码 1，但没有进入 promtool 测试。
- 影响：Dashboard 提交前质量门被阻止；未影响集群中的现有工作负载。
- 诊断证据：
  - Dashboard 清单使用 `}) or vector(0)`；
  - 测试脚本期望字符串错误地使用了 `})  or vector(0)`；
  - 独立运行 `grafana-dashboard-promql-test.yaml` 返回 `SUCCESS`；
  - `bash -x` 显示脚本停止在 Dashboard JSON 的 jq 契约断言。
- 原因：测试期望字符串在 `or` 前多了一个空格，精确字符串比较失败。
- 解决方案：将测试期望字符串改为与 Dashboard 清单一致的单空格形式。
- 验证方法：
  - `apps/sre-agent-v2/scripts/test-grafana-dashboard.sh`
  - `apps/sre-agent-v2/scripts/test-prometheus-rules.sh`
  - Kubernetes 服务端 dry-run。
- 验证结果：Dashboard 契约测试、PromQL 语义测试、规则测试和服务端 dry-run 均通过。
- 预防措施：
  - 修改 PromQL 后同时检查清单、契约测试和 PromQL 测试中的表达式；
  - 无输出失败时使用 `bash -x` 定位脚本停止位置；
  - 使用独立 `promql_expr_test` 验证表达式语义，不只依赖字符串比较。

## ERR-20260919-004：Argo CD 验证轮询结束后才完成目标 revision 调谐

- 日期：2026-09-17
- 完成日期：2026-09-19
- 环境：Kubernetes 开发集群，Argo CD Application `sre-agent-v2-shadow`
- 状态：已解决
- 报错问题：连续 36 次轮询后，Application 仍显示旧 revision，验证脚本输出 `STOP: Argo CD did not reach the expected revision`。
- 影响：Dashboard 部署验收被暂停；没有证据表明 Argo CD 同步失败或现有工作负载不可用。
- 诊断证据：
  - 本地、tracking branch 和远端 `main` 均为目标提交；
  - 后续检查时 Application 已切换到目标 revision；
  - Application 为 `Synced/Healthy`，`conditions=[]`；
  - Operation 状态为 `Succeeded`，消息为 `successfully synced (all tasks run)`；
  - Dashboard ConfigMap 已由 Argo CD 创建。
- 原因：验证轮询窗口在 Argo CD 完成下一次 Git 刷新和自动调谐前结束；没有发现仓库认证、清单生成或同步错误。
- 解决方案：先执行只读检查确认远端 revision、Application conditions、operation state 和 Events；未执行 hard refresh，等待后续自动调谐完成。
- 验证方法：
  - 对比本地、远端和 Application revision；
  - 检查 Application sync、health、conditions 和 operation state；
  - 检查 Dashboard ConfigMap、sidecar 文件和 Grafana API。
- 验证结果：
  - Argo CD 已调谐到目标 revision；
  - Dashboard ConfigMap 已创建；
  - Grafana sidecar 已发现 Dashboard；
  - Grafana API 返回 `provisioned=true`；
  - Prometheus datasource health 为 `OK`。
- 预防措施：
  - 轮询超时后先检查远端 revision 和 Argo CD conditions，不直接判定部署失败；
  - 对正常 Git 轮询使用更长等待窗口或逐步退避；
  - 只有确认缓存长期不更新且不存在仓库错误时，才考虑受控 hard refresh；
  - 区分“验证窗口结束”和“同步操作失败”。
