"""Local offline CLI. Principal attributes are trusted inputs, not authentication."""

import argparse
import json
from pathlib import Path
import sys

from rag_mvp.corpus import ROOT, load_manifest
from rag_mvp.retrieval import Principal


def main():
    parser = argparse.ArgumentParser(description="Offline knowledge-context keyword/BM25 retrieval")
    parser.add_argument("query")
    parser.add_argument("--mode", choices=("production", "evaluation"), default="production")
    parser.add_argument("--manifest", type=Path, default=ROOT / "docs/rag/corpus-manifest-v1.json")
    parser.add_argument("--tenant", default="sre-agent")
    parser.add_argument("--subject", default="local-learner")
    parser.add_argument("--acl", action="append", default=[])
    parser.add_argument("--algorithm", choices=("keyword", "bm25"), default="keyword")
    args = parser.parse_args()
    try:
        retriever = load_manifest(args.manifest, mode=args.mode, algorithm=args.algorithm)
        context = retriever.retrieve(args.query, Principal(args.subject, args.tenant, frozenset(args.acl)))
    except (ValueError, KeyError, TypeError, OSError) as error:
        print("RAG_INPUT_ERROR: " + str(error), file=sys.stderr)
        return 2
    print(json.dumps(context, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
