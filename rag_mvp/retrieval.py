"""Offline keyword/BM25 retrieval behind Retrieve(query, principal)."""

from collections import Counter
from dataclasses import dataclass
from datetime import datetime, timezone
from hashlib import sha256
from math import log1p, sqrt
import re


@dataclass(frozen=True)
class Principal:
    subject: str
    tenant: str
    acl: frozenset[str]


@dataclass(frozen=True)
class Document:
    document_id: str
    version: int
    tenant: str
    acl: frozenset[str]
    status: str
    review_due_at: str
    source_locator: str
    text: str
    evaluation_approved_hash: str | None = None

    @property
    def content_hash(self):
        return sha256(self.text.encode("utf-8")).hexdigest()


def _tokens(text):
    tokens = []
    for part in re.findall(r"[a-z0-9_]+|[\u3400-\u9fff]+", text.casefold()):
        if re.fullmatch(r"[\u3400-\u9fff]+", part) and len(part) > 1:
            tokens.extend(part[i:i + 2] for i in range(len(part) - 1))
        else:
            tokens.append(part)
    return tokens


def _sections(text):
    text = text.replace("\r\n", "\n")
    if text.startswith("---\n"):
        parts = text.split("\n---\n", 1)
        text = parts[1] if len(parts) == 2 else ""
    headings, buffer = [], []
    fence = None
    for line in text.splitlines():
        marker = re.match(r"^\s*(`{3,}|~{3,})", line)
        if marker:
            mark = marker[1]
            if fence is None:
                fence = (mark[0], len(mark))
            elif mark[0] == fence[0] and len(mark) >= fence[1]:
                fence = None
            buffer.append(line)
            continue
        heading = re.match(r"^(#{1,6})\s+(.+)$", line) if fence is None else None
        if heading:
            if any(part.strip() and not re.match(r"^#{1,6}\s", part) for part in buffer):
                yield "/".join(title for _, title in headings) or "document", "\n".join(buffer).strip()
            level, title = len(heading[1]), heading[2].strip()
            while headings and headings[-1][0] >= level:
                headings.pop()
            headings.append((level, title))
            buffer = [line]
        else:
            buffer.append(line)
    if any(part.strip() and not re.match(r"^#{1,6}\s", part) for part in buffer):
        yield "/".join(title for _, title in headings) or "document", "\n".join(buffer).strip()


class Retriever:
    def __init__(self, documents, *, clock=None, mode="production", algorithm="keyword"):
        if mode not in {"production", "evaluation"}:
            raise ValueError("mode must be production or evaluation")
        if algorithm not in {"keyword", "bm25"}:
            raise ValueError("algorithm must be keyword or bm25")
        self._documents = tuple(documents)
        self._clock = clock or (lambda: datetime.now(timezone.utc))
        self._mode = mode
        self._algorithm = algorithm

    def retrieve(self, query, principal, *, limit=5):
        now = self._clock()
        terms = set(_tokens(query))
        chunks, hits = [], []
        for doc in self._documents:
            if doc.tenant != principal.tenant or not doc.acl or not (doc.acl & principal.acl):
                continue
            evaluation_allowed = (
                self._mode == "evaluation"
                and doc.status in {"generated_draft", "source_verified", "human_approved"}
                and doc.evaluation_approved_hash == doc.content_hash
            )
            if doc.status != "published" and not evaluation_allowed:
                continue
            try:
                due = datetime.fromisoformat(doc.review_due_at)
            except (ValueError, TypeError):
                continue
            if due.tzinfo is None or now >= due:
                continue
            for index, (section, text) in enumerate(_sections(doc.text)):
                words = Counter(_tokens(text))
                if words:
                    chunks.append((doc, index, section, text, words))
        # Statistics include only eligible chunks, never hidden/expired documents.
        if self._algorithm == "bm25" and chunks:
            count = len(chunks)
            frequencies = Counter(term for *_, words in chunks for term in words)
            average_length = sum(sum(words.values()) for *_, words in chunks) / count
        for doc, index, section, text, words in chunks:
            common = terms & words.keys()
            if not common:
                continue
            if self._algorithm == "keyword":
                score = len(common) / sqrt(len(words))
            else:
                # Positive-IDF BM25, fixed k1=1.2 and b=0.75; length counts repetitions.
                norm = 1.2 * (0.25 + 0.75 * sum(words.values()) / average_length)
                score = sum(
                    log1p((count - frequencies[term] + 0.5) / (frequencies[term] + 0.5))
                    * words[term] * 2.2 / (words[term] + norm)
                    for term in sorted(common)
                )
            chunk_id = sha256(
                f"{doc.document_id}\0{doc.version}\0{section}\0{index}\0{text}".encode("utf-8")
            ).hexdigest()
            hits.append((score, doc.document_id, index, {
                "statement": text,
                "scope": "knowledge_context_only",
                "uncertainty": "limited",
                "citations": [{
                    "document_id": doc.document_id,
                    "document_version": doc.version,
                    "section_path": section,
                    "chunk_id": "sha256:" + chunk_id,
                    "source_locator": doc.source_locator,
                }],
            }))
        hits.sort(key=lambda hit: (-hit[0], hit[1], hit[2]))
        return {
            "schema_version": "knowledge-context/v1",
            "query_id": "query-" + sha256(
                f"{principal.tenant}\0{principal.subject}\0{query}".encode("utf-8")
            ).hexdigest()[:24],
            "generated_at": now.isoformat(),
            "items": [hit[3] for hit in hits[:limit]],
        }
