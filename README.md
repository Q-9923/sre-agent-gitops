# Kubernetes SRE 智能运维助手｜未来杯演示源码

这是可以在评审机器上运行的独立演示。不需要参赛团队的电脑、虚拟机、Kubernetes、私网模型或密钥。

## 启动

在本目录（能看到 `web/` 和 `rag_mvp/`）执行：

```sh
docker build -f web/contest/Dockerfile -t sre-contest-demo:20261117 .
docker run --rm -p 127.0.0.1:8876:8080 sre-contest-demo:20261117
```

打开 [本地演示](http://127.0.0.1:8876/)；[健康检查](http://127.0.0.1:8876/healthz) 应返回 `code: OK` 和 `mode: contest_public_sample`。首次构建需要联网下载依赖；演示运行不调用外部模型或集群。

不用 Docker 时，也可使用 Python 3.12、Node.js 24 和 pnpm 11.25.0。在已启用的 Python 虚拟环境中，从本目录执行：

```sh
python -m pip install -r web/contest/requirements.txt
pnpm --dir web install --frozen-lockfile
pnpm --dir web run build:contest
python -m uvicorn web.contest.app:app --host 127.0.0.1 --port 8876 --no-access-log
```

## 复现演示

1. 在样例检索中输入「Pod UID 变化后旧审批还能用吗？」。选择 BM25，打开命中引用，核对教学文档中“旧审批不能复用”的依据。
2. 在依据对照中比较完整五段与双定义两段，检查两段中是否同时保留 Claim 和 Approval 的直接定义。
3. 在编排预览中依次运行有依据、无依据、模拟超时三个场景，观察实际执行的 LangGraph 节点和结束原因。

本演示实际运行检索和 LangGraph，并使用模拟模型。它不连接真实集群，不执行运维写操作，也不是实测多 Agent 反馈闭环；依据对照不展示虚构的模型耗时或生成效果。
