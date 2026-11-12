"""Read only the explicit corpus allowlist and verify version/content bindings."""

from hashlib import sha256
import json
from pathlib import Path

from rag_mvp.retrieval import Document, Retriever


ROOT = Path(__file__).resolve().parent.parent


def _inside(relative, allowed):
    path = (ROOT / relative).resolve()
    if not path.is_relative_to(allowed.resolve()):
        raise ValueError("source path outside allowed project directory")
    return path


def load_manifest(path, *, mode="production", clock=None, algorithm="keyword"):
    manifest = json.loads(Path(path).read_text(encoding="utf-8"))
    if manifest.get("schema_version") != "rag-corpus-manifest/v1":
        raise ValueError("unsupported corpus manifest")
    documents, identities = [], set()
    for entry in manifest["documents"]:
        identity = entry["document_id"]
        version = entry["document_version"]
        if identity in identities or type(version) is not int or version < 1:
            raise ValueError("duplicate document identity or invalid version")
        identities.add(identity)
        source = _inside(entry["corpus_path"], ROOT / "docs/rag/corpus")
        raw = source.read_bytes()
        digest = sha256(raw).hexdigest()
        if digest != entry["content_sha256"]:
            raise ValueError("corpus hash mismatch: " + identity)
        text = raw.decode("utf-8")
        front, separator, _ = text.partition("\n---\n")
        if not text.startswith("---\n") or not separator:
            raise ValueError("corpus front matter missing")
        metadata = dict(line.split(": ", 1) for line in front.splitlines()[1:] if ": " in line)
        if (metadata.get("document_id") != identity
                or metadata.get("document_version") != str(version)
                or metadata.get("status") != entry["review_status"]
                or metadata.get("allowed_output") != "knowledge_context_only"
                or metadata.get("acl") not in entry["acl"]):
            raise ValueError("corpus metadata mismatch: " + identity)
        for reference in entry["derived_from"]:
            evidence = _inside(reference["path"], ROOT / "docs")
            if sha256(evidence.read_bytes()).hexdigest() != reference["sha256"]:
                raise ValueError("source evidence hash mismatch: " + identity)
        approval = entry.get("evaluation_approval") or {}
        approved_hash = None
        if (approval.get("scope") == "isolated_evaluation_only"
                and approval.get("approved_by")
                and approval.get("approved_on")
                and approval.get("content_sha256") == digest):
            approved_hash = digest
        if entry["review_status"] == "published":
            production = entry.get("production_approval") or {}
            if (not production.get("approved_by")
                    or production["approved_by"] == entry.get("author")
                    or production.get("content_sha256") != digest
                    or not production.get("published_batch_id")):
                raise ValueError("published corpus lacks distinct hash-bound approval")
        documents.append(Document(
            document_id=identity, version=version, tenant=entry["tenant"],
            acl=frozenset(entry["acl"]), status=entry["review_status"],
            review_due_at=entry["review_due_at"], source_locator=entry["corpus_path"],
            text=text, evaluation_approved_hash=approved_hash,
        ))
    return Retriever(documents, mode=mode, clock=clock, algorithm=algorithm)
