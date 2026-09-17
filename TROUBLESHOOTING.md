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
