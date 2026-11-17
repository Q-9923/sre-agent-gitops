"""Public contest workspace: only the shipped, illustrative sample corpus."""

from importlib.metadata import version
import json
from pathlib import Path
from time import perf_counter
from typing import Literal
from uuid import uuid4

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, ConfigDict, Field
from starlette.exceptions import HTTPException

from rag_mvp.generation import ModelError
from rag_mvp.retrieval import Document, Principal, Retriever
from rag_mvp.workflow import KnowledgeWorkflow


HERE = Path(__file__).resolve().parent
DIST = HERE.parent / "dist-contest"
SAMPLE_DATE = "2026-11-17"
ACL = frozenset({"contest:public-sample"})
READER = Principal("visitor", "contest-demo", ACL)
COMPARISON_QUESTION = "Claim 与 Approval 有什么区别？"
SAMPLES = {
    "demo-approval": "# Approval：审批与目标身份\n\n"
    "## Pod UID 变化后能否沿用旧审批\n"
    "同名 Pod 可能已经被替换。Pod UID 变化后，旧审批不能复用；"
    "需要按新目标身份和候选方案重新核对 Approval。\n\n"
    "## Approval 的定义\n"
    "Approval 是获授权者对指定事件、候选方案和目标身份的限时许可，"
    "不表示动作已经执行。Claim 本身不产生 Approval。\n",
    "demo-claim": "# Claim：事件处理权\n\n"
    "## Claim 的定义\n"
    "Claim 决定当前谁能在租约期内处理一个活动事件。"
    "这不是审批，也不授予 Kubernetes 写权限。\n\n"
    "## 租约与隔离令牌\n"
    "Lease 限定处理权的时间；续约失败或租约过期后，"
    "隔离令牌可用来拒绝旧持有者的迟到写入。Claim 不是 Approval。\n",
    "demo-grafana": "# Grafana 入口排查（教学示例）\n\n"
    "## 浏览器无法访问要重装吗\n"
    "先辨别访问地址、服务入口和后端端点。"
    "单次连接失败不能证明应用损坏，不应直接重装。\n\n"
    "## 只读补证和复验\n"
    "检查 EndpointSlice 能补充后端端点是否可用的证据；"
    "端点可用不等于公网入口已恢复。需要重新验证入口。\n",
    "demo-evidence": "# 证据与许可（教学示例）\n\n"
    "## 文档不是现场证据\n"
    "Claim、Approval 等术语可由知识库解释，但知识片段不是现场状态，"
    "不能当作执行授权或故障恢复的证明。\n",
}
DOCUMENTS = {
    key: Document(key, 1, READER.tenant, ACL, "published",
                  "2099-01-01T00:00:00+00:00", f"contest-sample/{key}", text)
    for key, text in SAMPLES.items()
}
RETRIEVERS = {
    algorithm: Retriever(DOCUMENTS.values(), algorithm=algorithm)
    for algorithm in ("bm25", "keyword")
}
app = FastAPI(title="SRE contest demo", docs_url=None, redoc_url=None, openapi_url=None)


def envelope(request, data=None, *, code="OK", status=200):
    return JSONResponse(status_code=status, content={
        "request_id": request.state.request_id, "code": code,
        "message": "success" if code == "OK" else code, "data": data,
        "errors": [] if code == "OK" else [{"code": code}],
    })


@app.middleware("http")
async def common_headers(request, call_next):
    request.state.request_id = f"req-{uuid4().hex}"
    response = await call_next(request)
    response.headers["X-Content-Type-Options"] = "nosniff"
    response.headers["Referrer-Policy"] = "no-referrer"
    return response


@app.exception_handler(RequestValidationError)
async def invalid_input(request, _error):
    return envelope(request, code="INVALID_INPUT", status=422)


@app.exception_handler(HTTPException)
async def rejected(request, error):
    return envelope(request, code="NOT_FOUND" if error.status_code == 404 else "REQUEST_REJECTED",
                    status=error.status_code)


class RetrievalInput(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)
    query: str = Field(min_length=1, max_length=500, pattern=r"\S")
    algorithm: Literal["bm25", "keyword"] = "bm25"


@app.post("/api/v1/retrieve")
def retrieve(body: RetrievalInput, request: Request):
    start = perf_counter()
    context = RETRIEVERS[body.algorithm].retrieve(body.query, READER)
    return envelope(request, {
        "context": context, "algorithm": body.algorithm, "model": "not_connected",
        "elapsed_ms": round((perf_counter() - start) * 1000, 2),
        "timing_scope": "server_retrieval_only",
    })


@app.get("/api/v1/documents/{document_id}")
def document(document_id: str, request: Request):
    doc = DOCUMENTS.get(document_id)
    if doc is None:
        return envelope(request, code="NOT_FOUND", status=404)
    return envelope(request, {
        "id": doc.document_id, "title": doc.text.splitlines()[0][2:], "text": doc.text,
        "version": doc.version, "content_sha256": doc.content_hash,
        "source_locator": doc.source_locator, "source_kind": "public_demo_sample",
    })


@app.get("/api/v1/evidence-comparison")
def evidence_comparison(request: Request):
    items = RETRIEVERS["bm25"].retrieve(COMPARISON_QUESTION, READER)["items"]
    direct = [item for item in items
              if item["citations"][0]["section_path"].rsplit("/", 1)[-1]
              in {"Claim 的定义", "Approval 的定义"}]

    def variant(label, candidates):
        return {"label": label,
                "context_characters": sum(len(item["statement"]) for item in candidates),
                "citations": [item["citations"][0] for item in candidates]}

    return envelope(request, {
        "kind": "public_evidence_comparison", "question": COMPARISON_QUESTION,
        "model_called": False, "variants": [variant("完整五段", items), variant("双定义两段", direct)],
    })


class PreviewInput(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)
    scenario: Literal["answered", "no_evidence", "simulated_timeout"]


@app.post("/api/v1/workflow-preview")
def workflow_preview(body: PreviewInput, request: Request):
    calls = []

    def synthetic_model(_query, sources):
        calls.append(len(sources))
        if body.scenario == "simulated_timeout":
            raise ModelError("MODEL_TIMEOUT")
        return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                           "answer": "仅用于公开编排预览的模拟回答。", "source_ids": ["S1"]})

    query = "qxzvnonmatching" if body.scenario == "no_evidence" else COMPARISON_QUESTION
    outcome = KnowledgeWorkflow(RETRIEVERS["bm25"], synthetic_model).answer(query, READER)
    return envelope(request, {
        "schema_version": "knowledge-workflow-demo/v1", "langgraph": version("langgraph"),
        "evaluation_as_of": SAMPLE_DATE, "model": "synthetic_only",
        "run": {
            "case": body.scenario, "trace": outcome["trace"], "status": outcome["result"]["status"],
            "error_code": outcome["error_code"], "citation_count": len(outcome["result"]["citations"]),
            "synthetic_model_candidate_counts": calls,
        },
    })


@app.get("/healthz")
def health(request: Request):
    if not (DIST / "contest.html").is_file():
        return envelope(request, code="CONTEST_BUILD_REQUIRED", status=503)
    return envelope(request, {"mode": "contest_public_sample", "model": "not_connected"})


@app.get("/")
def index(request: Request):
    if not (DIST / "contest.html").is_file():
        return envelope(request, code="CONTEST_BUILD_REQUIRED", status=503)
    return FileResponse(DIST / "contest.html")


if (DIST / "assets").is_dir():
    app.mount("/assets", StaticFiles(directory=DIST / "assets"), name="contest_assets")
