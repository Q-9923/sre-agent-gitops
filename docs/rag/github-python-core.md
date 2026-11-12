# Read-only RAG and LangGraph core

This source-only slice contains local BM25 retrieval, versioned citations, read-only answer validation, an explicit Ollama adapter, and an optional LangGraph workflow. It does not include an approved knowledge corpus, live endpoints, runtime data, web assets, or Kubernetes execution. Synthetic tests do not contact a model or cluster.

From the repository root with Python 3.12:

```text
python -B -m unittest rag_mvp.test_retrieval rag_mvp.test_bm25 rag_mvp.test_generation
python -m venv ../rag-core-venv
```

In the virtual environment, install the verified Python dependency lock and run the Graph contract tests:

```text
python -m pip install -r rag_mvp/requirements-langgraph-lock.txt
python -B -m unittest scripts.rag.test_readonly_workflow
```

`Retriever.retrieve(query, principal)` only returns eligible Knowledge Context. `Answerer.answer(query, principal)` validates a model draft against the already retrieved versioned citations. `KnowledgeWorkflow.answer(query, principal)` uses the same validation and returns the actual local node trace. Retrieval, model access, and deployment remain separate decisions. The optional file-based CLIs require your own registered manifest and local source files; do not substitute real runbooks or credentials for the synthetic fixtures.
