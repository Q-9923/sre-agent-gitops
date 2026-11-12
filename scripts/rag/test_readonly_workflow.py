"""Opt-in LangGraph acceptance tests; no real model or network transport."""

from dataclasses import replace
import json
import os
import unittest
from unittest.mock import patch

from langsmith.utils import tracing_is_enabled

from rag_mvp.generation import Answerer, ModelError
from rag_mvp.retrieval import Retriever
from rag_mvp.test_retrieval import NOW, PRINCIPAL, document
from rag_mvp.workflow import KnowledgeWorkflow


class ObservedRetriever(Retriever):
    def __init__(self, documents):
        super().__init__(documents, clock=lambda: NOW, algorithm="bm25")
        self.requests = []

    def retrieve(self, query, principal, *, limit=5):
        self.requests.append((query, principal, limit))
        return super().retrieve(query, principal, limit=limit)


class ReadOnlyWorkflowTests(unittest.TestCase):
    def test_one_top_five_retrieval_preserves_fifth_versioned_citation(self):
        reader = ObservedRetriever([
            document(document_id=f"fixture:{n}", text="Grafana reference") for n in range(1, 7)
        ])
        model_calls = []

        def model(query, sources):
            model_calls.append((query, len(sources)))
            return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                               "answer": "Synthetic Grafana reference.", "source_ids": ["S5"]})

        answer = KnowledgeWorkflow(reader, model).answer("Grafana", PRINCIPAL)
        self.assertEqual(reader.requests, [("Grafana", PRINCIPAL, 5)])
        self.assertEqual(model_calls, [("Grafana", 5)])
        self.assertEqual(answer["schema_version"], "knowledge-workflow/v1")
        self.assertEqual(answer["trace"], ["retrieve", "compose"])
        result = answer["result"]
        self.assertEqual((result["status"], result["scope"]), ("ANSWERED", "knowledge_context_only"))
        citation, = result["citations"]
        self.assertEqual(citation["document_id"], "fixture:5")
        self.assertEqual(citation["document_version"], 1)
        self.assertTrue(citation["chunk_id"].startswith("sha256:"))

    def test_acl_empty_result_ends_without_model_and_does_not_reuse_previous_context(self):
        reader = ObservedRetriever([document()])
        calls = []

        def model(query, sources):
            calls.append(query)
            return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                               "answer": "Synthetic reference.", "source_ids": ["S1"]})

        workflow = KnowledgeWorkflow(reader, model)
        self.assertEqual(workflow.answer("Grafana", PRINCIPAL)["result"]["status"], "ANSWERED")
        result = workflow.answer("Grafana", replace(PRINCIPAL, acl=frozenset()))
        self.assertEqual(result["trace"], ["retrieve", "no_evidence"])
        self.assertEqual(result["error_code"], "NO_EVIDENCE")
        self.assertEqual(result["result"]["citations"], [])
        self.assertEqual(result["result"]["status"], "INSUFFICIENT_EVIDENCE")
        self.assertEqual(calls, ["Grafana"])

    def test_timeout_ends_without_retry_or_citations(self):
        calls = []

        def timed_out(query, sources):
            calls.append(query)
            raise ModelError("MODEL_TIMEOUT")

        result = KnowledgeWorkflow(ObservedRetriever([document()]), timed_out).answer("Grafana", PRINCIPAL)
        self.assertEqual(calls, ["Grafana"])
        self.assertEqual(result["trace"], ["retrieve", "compose"])
        self.assertEqual(result["error_code"], "MODEL_TIMEOUT")
        self.assertEqual(result["result"]["status"], "FAILED")
        self.assertEqual((result["result"]["answer"], result["result"]["citations"]), ("", []))

    def test_retrieval_error_ends_without_generating_or_returning_raw_error(self):
        class UnavailableRetriever:
            def retrieve(self, query, principal, *, limit):
                raise OSError("synthetic raw detail")

        def model(query, sources):
            self.fail("Generation must not run if retrieval fails")

        result = KnowledgeWorkflow(UnavailableRetriever(), model).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["trace"], ["retrieve", "retrieval_failed"])
        self.assertEqual(result["error_code"], "RETRIEVAL_FAILED")
        self.assertIsNone(result["result"])
        self.assertNotIn("synthetic raw detail", json.dumps(result))

    def test_budget_rejects_without_calling_model(self):
        def model(query, sources):
            self.fail("Generation must not run when the budget is exceeded")

        result = KnowledgeWorkflow(ObservedRetriever([document()]), model, max_context_chars=1).answer(
            "Grafana", PRINCIPAL)
        self.assertEqual(result["error_code"], "CONTEXT_BUDGET_EXCEEDED")
        self.assertEqual(result["result"]["citations"], [])

    def test_shared_answer_contract_rejects_fabricated_citation(self):
        def model(query, sources):
            return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                               "answer": "Synthetic raw draft.", "source_ids": ["S6"]})

        reader = ObservedRetriever([document()])
        expected = Answerer(reader, model).answer("Grafana", PRINCIPAL)
        result = KnowledgeWorkflow(reader, model).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["result"], expected)
        self.assertEqual(result["error_code"], "MODEL_OUTPUT_INVALID")
        self.assertNotIn("Synthetic raw draft.", json.dumps(result))

    def test_local_trace_does_not_enable_external_tracing(self):
        def model(query, sources):
            self.assertFalse(tracing_is_enabled())
            return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                               "answer": "Synthetic reference.", "source_ids": ["S1"]})

        with patch.dict(os.environ, {"LANGSMITH_TRACING": "true"}):
            result = KnowledgeWorkflow(ObservedRetriever([document()]), model).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["trace"], ["retrieve", "compose"])
        self.assertEqual(result["error_code"], "OK")
