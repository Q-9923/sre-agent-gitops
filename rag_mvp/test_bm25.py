"""Ranking behavior through the public retrieval interface."""

import unittest

from rag_mvp import test_retrieval as shared
from rag_mvp.retrieval import Retriever
from rag_mvp.test_retrieval import NOW, PRINCIPAL, document


class BM25BoundaryTests(shared.RetrievalTests):
    algorithm = "bm25"


class BM25Tests(unittest.TestCase):
    def test_rare_query_term_outranks_common_term_in_equal_length_chunks(self):
        docs = [
            document(document_id="fixture:a-common", text="common alpha"),
            document(document_id="fixture:b-common", text="common beta"),
            document(document_id="fixture:z-rare", text="rare gamma"),
        ]
        baseline = Retriever(docs, clock=lambda: NOW).retrieve("common rare", PRINCIPAL)
        result = Retriever(docs, clock=lambda: NOW, algorithm="bm25").retrieve(
            "common rare", PRINCIPAL
        )
        self.assertEqual(baseline["items"][0]["citations"][0]["document_id"], "fixture:a-common")
        self.assertEqual(result["items"][0]["citations"][0]["document_id"], "fixture:z-rare")

    def test_more_occurrences_win_at_equal_length_and_shorter_wins_at_equal_frequency(self):
        cases = (
            ("target filler filler filler", "target target target filler"),
            ("target filler filler filler", "target"),
        )
        for low, high in cases:
            with self.subTest(low=low, high=high):
                docs = [document(document_id="fixture:a-low", text=low),
                        document(document_id="fixture:z-high", text=high)]
                result = Retriever(docs, clock=lambda: NOW, algorithm="bm25").retrieve("target", PRINCIPAL)
                self.assertEqual(result["items"][0]["citations"][0]["document_id"], "fixture:z-high")

    def test_ineligible_documents_cannot_change_visible_ranking_statistics(self):
        visible = [document(document_id="fixture:a-common", text="common alpha"),
                   document(document_id="fixture:b-common", text="common beta"),
                   document(document_id="fixture:z-rare", text="rare gamma")]
        baseline = Retriever(visible, clock=lambda: NOW, algorithm="bm25").retrieve("common rare", PRINCIPAL)
        blocked = (
            {"acl": frozenset({"private:ops"})}, {"tenant": "another-project"},
            {"acl": frozenset()}, {"status": "generated_draft"}, {"status": "retired"},
            {"review_due_at": NOW.isoformat()}, {"review_due_at": "invalid"},
        )
        for changes in blocked:
            with self.subTest(changes=changes):
                # If included in DF/length statistics, these chunks make rare common.
                hidden = [document(document_id=f"fixture:hidden-{i}", text="rare " * 30, **changes)
                          for i in range(12)]
                result = Retriever(visible + hidden, clock=lambda: NOW, algorithm="bm25").retrieve(
                    "common rare", PRINCIPAL
                )
                self.assertEqual(result, baseline)

    def test_unknown_algorithm_is_rejected_instead_of_silently_using_another(self):
        with self.assertRaisesRegex(ValueError, "algorithm"):
            Retriever([], algorithm="unknown")


if __name__ == "__main__":
    unittest.main()
