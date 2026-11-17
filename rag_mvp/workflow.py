"""Opt-in local LangGraph orchestration; no tools, transport, or persistence."""

from langgraph.graph import END, START, StateGraph
from langsmith import tracing_context
from typing_extensions import NotRequired, TypedDict

from rag_mvp.generation import _compose_answer
from rag_mvp.retrieval import Principal


class _State(TypedDict):
    query: str
    principal: Principal
    context: NotRequired[dict | None]
    result: NotRequired[dict | None]
    error_code: NotRequired[str]


class KnowledgeWorkflow:
    """One Top-5 request, with the existing read-only answer and local node names."""

    def __init__(self, retriever, generate, *, max_context_chars=12000):
        if type(max_context_chars) is not int or max_context_chars < 1:
            raise ValueError("max_context_chars must be a positive integer")
        self._retriever = retriever
        self._generate = generate
        self._max_context_chars = max_context_chars
        graph = StateGraph(_State)
        graph.add_node("retrieve", self._retrieve)
        graph.add_node("compose", self._compose)
        graph.add_node("no_evidence", self._compose)
        graph.add_node("retrieval_failed", self._retrieval_failed)
        graph.add_edge(START, "retrieve")
        graph.add_conditional_edges("retrieve", self._route, {
            "compose": "compose",
            "no_evidence": "no_evidence",
            "retrieval_failed": "retrieval_failed",
        })
        for terminal in ("compose", "no_evidence", "retrieval_failed"):
            graph.add_edge(terminal, END)
        self._graph = graph.compile()

    def answer(self, query, principal):
        trace = []
        terminal = None
        # Do not publish query or context through inherited LangSmith settings.
        with tracing_context(enabled=False):
            for event in self._graph.stream({"query": query, "principal": principal},
                                            stream_mode="updates", version="v2"):
                for node, update in event["data"].items():
                    trace.append(node)
                    if "result" in update:
                        terminal = update
        return {"schema_version": "knowledge-workflow/v1", "trace": trace, **terminal}

    def _retrieve(self, state):
        try:
            return {"context": self._retriever.retrieve(state["query"], state["principal"], limit=5)}
        except (ValueError, KeyError, TypeError, OSError):
            return {"context": None}

    @staticmethod
    def _route(state):
        if state["context"] is None:
            return "retrieval_failed"
        return "compose" if state["context"]["items"] else "no_evidence"

    @staticmethod
    def _retrieval_failed(_state):
        return {"result": None, "error_code": "RETRIEVAL_FAILED"}

    def _compose(self, state):
        result = _compose_answer(state["query"], state["context"], self._generate,
                                 self._max_context_chars)
        return {"result": result, "error_code": result["error_code"]}
