# Kubernetes SRE 智能运维助手 · 未来杯公开体验版

这里是独立的匿名、只读演示入口。用自编教学样例实际运行 BM25/Keyword、核对命中原文、对比完整五段和双定义两段；LangGraph 执行成功、无依据、模拟超时三条路径。对照只计算依据片段和字符数，无真实模型调用、结果或耗时。未实现比赛要求的真实多 Agent 补证与复验闭环。

此入口不加载原项目知识清单、涉事日志、私网评测档案，也没有私网问答、集群、审批或执行接口。不应将原工作台或整个项目目录作为公网构建上下文。

## 本机使用

已有 Python 依赖和前端依赖时，从项目根执行：

```powershell
pnpm --dir web run build:contest
python -m uvicorn web.contest.app:app --host 127.0.0.1 --port 8876 --no-access-log
```

构建输出在 `web/dist-contest`，不替换原 `web/dist`。浏览器访问 [本地比赛版](http://127.0.0.1:8876/)。新环境安装依赖时，使用 `python -m pip install -r web/contest/requirements.txt` 及 `pnpm --dir web install --frozen-lockfile`。

当前 Windows 工作区已有两个隔离环境。也可从项目根运行下列命令，复用已安装的 LangGraph 和 FastAPI，不更改两套环境：

```powershell
& .\tmp\langgraph-local-20261112\Scripts\python.exe -B -c "import sys; sys.path.append('web/.venv/Lib/site-packages'); import uvicorn; uvicorn.run('web.contest.app:app', host='127.0.0.1', port=8876, access_log=False)"
```

## 托管候选（尚未上线）

从原项目根用 `web/contest/pack.ps1` 生成显式文件允许清单的源码 ZIP。不要把原项目直接连到比赛托管；解出该 ZIP 到独立目录，只从该目录构建：

```text
docker build -f web/contest/Dockerfile -t sre-contest-demo:20261117 .
docker run --rm -p 127.0.0.1:8876:8080 sre-contest-demo:20261117
```

源包可放入独立演示仓库。候选 Render Web Service 建议使用 Docker、上下文根目录、Dockerfile 路径 `web/contest/Dockerfile`、健康端点 `/healthz`。应用读取平台的 `PORT`，未设置时用 8080。不需配置模型密钥。首次部署应先确认端点返回 200，再用访客浏览器从其他网络完整点验；当前 Windows 无 Docker CLI，本地未运行容器构建或公网验收。

参考（2026-11-17）：[Docker 构建上下文](https://docs.docker.com/build/concepts/context/)、[Render Docker](https://render.com/docs/docker)、[Render 端口和健康检查设置](https://render.com/docs/web-services#port-binding)。
