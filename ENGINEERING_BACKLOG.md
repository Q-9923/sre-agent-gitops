# 可升级优化方向

## OPT-20260916-001：交付并接入 SRE Agent 运行指标

- 日期：2026-09-16
- 模块：可观测性 / Prometheus Metrics
- 优先级：P1
- 状态：待验收
- 现状：
  - 已在源码中增加统一 operational HTTP handler；
  - `/livez`、`/readyz` 与 `/metrics` 共用 HTTP 服务；
  - 已增加 Go Runtime Metrics；
  - 已增加 `sre_agent_cycles_total{result=...}` 业务计数器；
  - result 标签限制为固定集合，未知结果归一化为 `UNKNOWN`；
  - 尚未构建和部署本次 Metrics 版本镜像。
- 证据：
  - 单元测试、Race 测试、`go vet` 和构建通过；
  - 本地 Smoke Test 已观察到 `sre_agent_cycles_total{result="ERROR"} 1`；
  - `/livez` 返回 200，依赖失败时 `/readyz` 返回 503；
  - SIGTERM 后 operational HTTP 端口正常关闭。
- 影响：在完成集群交付前，Prometheus 尚不能持续采集生产 Shadow Agent 的运行指标，Grafana 和告警规则也无法使用这些指标。
- 优化方向：
  - 构建并推送不可变版本镜像；
  - 通过 GitOps 更新 Deployment；
  - 为 Metrics 暴露集群内 Service；
  - 根据现有 Prometheus Operator 部署方式增加 ServiceMonitor；
  - 后续补充 cycle 错误率、处理时延和决策结果等低基数指标。
- 验收标准：
  - Argo CD 显示目标 revision 为 `Synced/Healthy`；
  - Pod 使用预期镜像 digest 且无异常重启；
  - `/livez`、`/readyz`、`/metrics` 行为符合预期；
  - Prometheus Targets 中采集目标为 Up；
  - Prometheus 查询可以看到 `sre_agent_cycles_total`；
  - 指标标签保持低基数，不包含 Pod UID、incident ID 或错误文本。
