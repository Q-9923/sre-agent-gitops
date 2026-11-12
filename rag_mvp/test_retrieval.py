"""Synthetic fixtures exercise the public retrieval seam, not real approvals."""

import unittest
from dataclasses import replace
from datetime import datetime, timezone

from rag_mvp.retrieval import Document, Principal, Retriever


NOW = datetime(2026, 10, 30, tzinfo=timezone.utc)
PRINCIPAL = Principal("learner", "sre-agent", frozenset({"project:sre-agent"}))


def document(**changes):
    base = Document(
        document_id="fixture:grafana",
        version=1,
        tenant="sre-agent",
        acl=frozenset({"project:sre-agent"}),
        status="published",
        review_due_at="2026-11-30T00:00:00+00:00",
        source_locator="fixtures/grafana.md",
        text="# Grafana\n\nGrafana uses a single Web NodePort.\n",
    )
    return replace(base, **changes)


class RetrievalTests(unittest.TestCase):
    algorithm = "keyword"

    def retriever(self, documents, **options):
        return Retriever(documents, algorithm=self.algorithm, **options)

    def test_heading_only_fragment_does_not_consume_retrieval_slot(self):
        doc = document(text="# Grafana\n\n## Access\n\nGrafana browser uses NodePort.\n")
        result = self.retriever([doc], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL)
        self.assertEqual(len(result["items"]), 1)
        self.assertIn("Grafana browser uses NodePort.", result["items"][0]["statement"])

    def test_empty_or_unmatched_query_produces_no_invented_statement(self):
        for query in ("", "   ", "unfindable_token_98765"):
            with self.subTest(query=query):
                result = self.retriever([document()], clock=lambda: NOW).retrieve(query, PRINCIPAL)
                self.assertEqual(result["items"], [])
                self.assertEqual(set(result), {"schema_version", "query_id", "generated_at", "items"})

    def test_code_fences_remain_intact_and_version_changes_citation(self):
        text = "# Grafana\n\n```bash\n# not a section\necho Grafana\n```\n"
        first = self.retriever([document(text=text)], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL)
        second = self.retriever([document(text=text, version=2)], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL)
        self.assertIn("```bash\n# not a section\necho Grafana\n```", first["items"][0]["statement"])
        self.assertNotEqual(first["items"][0]["citations"][0]["chunk_id"], second["items"][0]["citations"][0]["chunk_id"])

    def test_chinese_query_returns_matching_section_not_unrelated_text(self):
        doc = document(text="# 手册\n\n## 网络\n\n浏览器入口使用节点端口。\n\n## 存储\n\n历史数据需要持久卷。\n")
        result = self.retriever([doc], clock=lambda: NOW).retrieve("浏览器为什么无法访问入口", PRINCIPAL)
        self.assertTrue(result["items"])
        self.assertNotIn("历史数据需要持久卷", result["items"][0]["statement"])
        self.assertEqual(result["items"][0]["citations"][0]["section_path"], "手册/网络")

    def test_evaluation_approval_is_hash_bound_and_never_enables_production(self):
        draft = document(status="generated_draft")
        approved = replace(draft, evaluation_approved_hash=draft.content_hash)
        self.assertEqual(self.retriever([approved], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL)["items"], [])
        self.assertTrue(self.retriever([approved], clock=lambda: NOW, mode="evaluation").retrieve("Grafana", PRINCIPAL)["items"])
        for blocked in (draft, replace(approved, text="Grafana changed"), replace(approved, status="retired")):
            with self.subTest(doc=blocked):
                self.assertEqual(self.retriever([blocked], clock=lambda: NOW, mode="evaluation").retrieve("Grafana", PRINCIPAL)["items"], [])

    def test_production_excludes_every_unpublished_state_and_due_document(self):
        for status in ("candidate", "generated_draft", "source_verified", "human_approved", "retired", "rejected", "unknown"):
            with self.subTest(status=status):
                result = self.retriever([document(status=status)], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL)
                self.assertEqual(result["items"], [])
        for due in ("2026-10-30T00:00:00+00:00", "2026-10-29T00:00:00+00:00", "", "not-a-date", "2026-11-30T00:00:00"):
            with self.subTest(due=due):
                result = self.retriever([document(review_due_at=due)], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL)
                self.assertEqual(result["items"], [])

    def test_acl_tenant_and_empty_acl_filter_before_top_k(self):
        public = document(text="Grafana public.")
        restricted = document(document_id="fixture:restricted", acl=frozenset({"secret:ops"}), text="Grafana " * 30)
        other_tenant = document(document_id="fixture:other-tenant", tenant="other", text="Grafana " * 40)
        no_acl = document(document_id="fixture:no-acl", acl=frozenset(), text="Grafana " * 50)
        baseline = self.retriever([public], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL, limit=1)
        actual = self.retriever([restricted, other_tenant, no_acl, public], clock=lambda: NOW).retrieve("Grafana", PRINCIPAL, limit=1)
        self.assertEqual(actual, baseline)

    def test_returns_exact_knowledge_with_stable_versioned_citations(self):
        retriever = self.retriever([document()], clock=lambda: NOW)
        first = retriever.retrieve("Grafana", PRINCIPAL)
        second = retriever.retrieve("Grafana", PRINCIPAL)
        self.assertEqual(first["schema_version"], "knowledge-context/v1")
        self.assertEqual(first, second)
        item = first["items"][0]
        self.assertIn("Grafana uses a single Web NodePort.", item["statement"])
        self.assertEqual(item["scope"], "knowledge_context_only")
        citation = item["citations"][0]
        self.assertEqual(citation["document_id"], "fixture:grafana")
        self.assertEqual(citation["document_version"], 1)
        self.assertEqual(citation["source_locator"], "fixtures/grafana.md")
        self.assertRegex(citation["chunk_id"], r"^sha256:[0-9a-f]{64}$")


if __name__ == "__main__":
    unittest.main()
