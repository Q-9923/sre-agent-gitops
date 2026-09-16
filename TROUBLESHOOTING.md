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
