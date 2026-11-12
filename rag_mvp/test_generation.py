"""Answer behavior through real retrieval and a synthetic external model seam."""

import json
import unittest
from dataclasses import replace

from rag_mvp.generation import Answerer
from rag_mvp.retrieval import Retriever
from rag_mvp.test_retrieval import NOW, PRINCIPAL, document


class AnswerTests(unittest.TestCase):
    def test_fifth_retrieved_source_is_not_silently_dropped_before_generation(self):
        docs = [document(document_id=f"fixture:{i}", text="Grafana reference") for i in range(1, 6)]

        def model(query, sources):
            self.assertEqual(len(sources), 5)
            return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                               "answer": "Grafana reference.", "source_ids": ["S5"]})

        result = Answerer(Retriever(docs, clock=lambda: NOW), model).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["status"], "ANSWERED")
        self.assertEqual(result["citations"][0]["document_id"], "fixture:5")

    def test_malformed_or_schema_invalid_drafts_are_redacted(self):
        good = {"schema_version": "answer-draft/v1", "status": "ANSWERED",
                "answer": "Grafana uses NodePort.", "source_ids": ["S1"]}
        invalid = ["not json", "[]", json.dumps(good) + " trailing",
                   '{"schema_version":"answer-draft/v1","schema_version":"answer-draft/v1"}',
                   '{"schema_version":NaN}',
                   json.dumps({k: v for k, v in good.items() if k != "answer"})]
        for changes in ({"schema_version": "unknown"}, {"action": "RESTART_POD"},
                        {"status": "EXECUTE"}, {"answer": " "}, {"answer": "x" * 4001},
                        {"answer": False}, {"source_ids": []}, {"source_ids": [True]},
                        {"source_ids": ["S1", "S1"]}, {"source_ids": "S1"},
                        {"status": "INSUFFICIENT_EVIDENCE"}):
            invalid.append(json.dumps({**good, **changes}))
        for raw in invalid:
            with self.subTest(raw_shape=raw[:35]):
                result = Answerer(Retriever([document()], clock=lambda: NOW),
                                  lambda query, sources: raw).answer("Grafana", PRINCIPAL)
                self.assertEqual(result["status"], "FAILED")
                self.assertEqual(result["error_code"], "MODEL_OUTPUT_INVALID")
                self.assertEqual(result["answer"], "")
                self.assertEqual(result["citations"], [])

    def test_model_can_abstain_even_when_retrieval_has_matches(self):
        raw = json.dumps({"schema_version": "answer-draft/v1", "status": "INSUFFICIENT_EVIDENCE",
                          "answer": "", "source_ids": []})
        result = Answerer(Retriever([document()], clock=lambda: NOW),
                          lambda query, sources: raw).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["status"], "INSUFFICIENT_EVIDENCE")
        self.assertEqual(result["error_code"], "MODEL_ABSTAINED")
        self.assertEqual(result["citations"], [])

    def test_ineligible_evidence_never_reaches_generation(self):
        def never_call(query, sources):
            raise AssertionError("Ineligible source reached model")

        for doc, reader in ((document(tenant="other"), PRINCIPAL),
                            (document(), replace(PRINCIPAL, acl=frozenset())),
                            (document(status="generated_draft"), PRINCIPAL),
                            (document(review_due_at=NOW.isoformat()), PRINCIPAL)):
            with self.subTest(doc=doc.document_id, reader=reader):
                result = Answerer(Retriever([doc], clock=lambda: NOW, mode="evaluation"),
                                  never_call).answer("Grafana", reader)
                self.assertEqual(result["error_code"], "NO_EVIDENCE")

    def test_context_over_budget_fails_without_sending_or_truncating_sources(self):
        def never_call(query, sources):
            raise AssertionError("Oversized context must not reach the model")

        retriever = Retriever([document(text="Grafana " * 100)], clock=lambda: NOW)
        result = Answerer(retriever, never_call, max_context_chars=200).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["status"], "FAILED")
        self.assertEqual(result["error_code"], "CONTEXT_BUDGET_EXCEEDED")
        self.assertEqual(result["answer"], "")

    def test_unknown_citation_is_rejected_without_returning_model_text(self):
        def model(query, sources):
            return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                               "answer": "untrusted private model text", "source_ids": ["S999"]})

        result = Answerer(Retriever([document()], clock=lambda: NOW), model).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["status"], "FAILED")
        self.assertEqual(result["error_code"], "MODEL_OUTPUT_INVALID")
        self.assertEqual(result["answer"], "")
        self.assertEqual(result["citations"], [])

    def test_answer_binds_model_source_ids_to_retrieved_versioned_citations(self):
        def model(query, sources):
            self.assertEqual(query, "Grafana")
            self.assertEqual(sources[0]["source_id"], "S1")
            self.assertIn("Grafana uses a single Web NodePort.", sources[0]["statement"])
            return json.dumps({"schema_version": "answer-draft/v1", "status": "ANSWERED",
                               "answer": "Grafana通过节点端口提供浏览器入口。", "source_ids": ["S1"]})

        retriever = Retriever([document()], clock=lambda: NOW, algorithm="bm25")
        result = Answerer(retriever, model).answer("Grafana", PRINCIPAL)
        self.assertEqual(result["status"], "ANSWERED")
        self.assertEqual(result["answer"], "Grafana通过节点端口提供浏览器入口。")
        self.assertEqual(result["error_code"], "OK")
        self.assertEqual(result["citations"][0]["document_id"], "fixture:grafana")
        self.assertEqual(result["citations"][0]["document_version"], 1)
        self.assertRegex(result["citations"][0]["chunk_id"], r"^sha256:[0-9a-f]{64}$")

    def test_no_evidence_abstains_without_calling_a_model(self):
        def never_call(query, sources):
            raise AssertionError("No evidence must not reach the model")

        retriever = Retriever([document()], clock=lambda: NOW, algorithm="bm25")
        result = Answerer(retriever, never_call).answer("unfindable_token_98765", PRINCIPAL)
        self.assertEqual(result["status"], "INSUFFICIENT_EVIDENCE")
        self.assertEqual(result["error_code"], "NO_EVIDENCE")
        self.assertEqual(result["citations"], [])
        self.assertEqual(result["scope"], "knowledge_context_only")


if __name__ == "__main__":
    unittest.main()
