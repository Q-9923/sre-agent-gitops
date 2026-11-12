"""Opt-in read-only QA CLI. No model call is allowed by default."""

import argparse
import json
from pathlib import Path
import sys

from rag_mvp.corpus import ROOT, load_manifest
from rag_mvp.generation import Answerer
from rag_mvp.ollama import OllamaGenerator
from rag_mvp.retrieval import Principal


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("query")
    parser.add_argument("--mode", choices=("production", "evaluation"), default="production")
    parser.add_argument("--manifest", type=Path, default=ROOT / "docs/rag/corpus-manifest-v1.json")
    parser.add_argument("--tenant", default="sre-agent")
    parser.add_argument("--subject", default="local-learner")
    parser.add_argument("--acl", action="append", default=[])
    parser.add_argument("--algorithm", choices=("keyword", "bm25"), default="bm25")
    parser.add_argument("--endpoint", help="Explicit Ollama HTTP(S) origin; never discovered automatically")
    parser.add_argument("--model", default="qwen3.5:4b-q4_K_M")
    parser.add_argument("--timeout", type=float, default=60)
    parser.add_argument("--max-context-chars", type=int, default=12000)
    parser.add_argument("--allow-model-call", action="store_true",
                        help="Explicitly acknowledge sending this query and eligible snippets to the model")
    parser.add_argument("--json", action="store_true", help="Print versioned machine-readable answer")
    args = parser.parse_args()
    if not args.allow_model_call:
        print("MODEL_CALL_NOT_APPROVED: explicit opt-in required; no model was contacted", file=sys.stderr)
        return 2
    if not args.endpoint:
        parser.error("--endpoint is required for an explicitly approved model call")
    try:
        model = OllamaGenerator(args.endpoint, args.model, timeout_seconds=args.timeout)
        retriever = load_manifest(args.manifest, mode=args.mode, algorithm=args.algorithm)
        answerer = Answerer(retriever, model.generate, max_context_chars=args.max_context_chars)
        result = answerer.answer(args.query, Principal(args.subject, args.tenant, frozenset(args.acl)))
    except (ValueError, KeyError, TypeError, OSError):
        print("RAG_INPUT_ERROR: check local configuration and verified corpus", file=sys.stderr)
        return 2
    if args.json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    elif result["status"] == "FAILED":
        print(result["error_code"], file=sys.stderr)
    else:
        print(result["answer"])
        for i, citation in enumerate(result["citations"], 1):
            print(f"[{i}] {citation['document_id']} v{citation['document_version']} | "
                  f"{citation['section_path']} | {citation['source_locator']} | {citation['chunk_id']}")
        print("仅为知识参考，不代表当前集群状态、审批或执行授权。")
    return {"ANSWERED": 0, "INSUFFICIENT_EVIDENCE": 3, "FAILED": 1}[result["status"]]


if __name__ == "__main__":
    raise SystemExit(main())
