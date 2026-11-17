"""Read-only answer composition; model text never grants execution authority."""

import json


DRAFT_SCHEMA = {
    "type": "object", "additionalProperties": False,
    "required": ["schema_version", "status", "answer", "source_ids"],
    "properties": {
        "schema_version": {"const": "answer-draft/v1"},
        "status": {"enum": ["ANSWERED", "INSUFFICIENT_EVIDENCE"]},
        "answer": {"type": "string", "maxLength": 4000},
        "source_ids": {"type": "array", "uniqueItems": True, "maxItems": 5,
                       "items": {"type": "string"}},
    },
}


class ModelError(Exception):
    """Only a stable redacted code crosses the external model seam."""

    def __init__(self, code):
        super().__init__(code)
        self.code = code


def parse_json(text):
    def unique_fields(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON field")
            result[key] = value
        return result

    def invalid_constant(value):
        raise ValueError("nonstandard JSON constant")

    return json.loads(text, object_pairs_hook=unique_fields, parse_constant=invalid_constant)


class Answerer:
    def __init__(self, retriever, generate, *, max_context_chars=12000):
        if type(max_context_chars) is not int or max_context_chars < 1:
            raise ValueError("max_context_chars must be a positive integer")
        self._retriever = retriever
        self._generate = generate
        self._max_context_chars = max_context_chars

    def answer(self, query, principal):
        context = self._retriever.retrieve(query, principal, limit=5)
        return _compose_answer(query, context, self._generate, self._max_context_chars)


def _compose_answer(query, context, generate, max_context_chars):
    """Validate a draft against a previously retrieved Knowledge Context."""
    result = {
        "schema_version": "knowledge-answer/v1",
        "query_id": context["query_id"],
        "scope": "knowledge_context_only",
        "status": "INSUFFICIENT_EVIDENCE",
        "answer": "当前可用知识不足，无法给出有依据的回答。",
        "citations": [],
        "error_code": "NO_EVIDENCE",
    }
    if not context["items"]:
        return result
    sources = [{"source_id": f"S{i}", "statement": item["statement"],
                "citations": item["citations"]}
               for i, item in enumerate(context["items"], 1)]
    if len(json.dumps({"question": query, "sources": sources}, ensure_ascii=False)) > max_context_chars:
        result.update(status="FAILED", answer="", error_code="CONTEXT_BUDGET_EXCEEDED")
        return result
    lookup = {source["source_id"]: source for source in sources}
    try:
        draft = parse_json(generate(query, sources))
        if (not isinstance(draft, dict)
                or set(draft) != {"schema_version", "status", "answer", "source_ids"}
                or draft["schema_version"] != "answer-draft/v1"
                or draft["status"] not in ("ANSWERED", "INSUFFICIENT_EVIDENCE")
                or not isinstance(draft["answer"], str) or len(draft["answer"]) > 4000
                or not isinstance(draft["source_ids"], list) or len(draft["source_ids"]) > 5
                or any(not isinstance(identity, str) or identity not in lookup
                       for identity in draft["source_ids"])
                or len(set(draft["source_ids"])) != len(draft["source_ids"])):
            raise ValueError("invalid answer draft")
        if draft["status"] == "INSUFFICIENT_EVIDENCE":
            if draft["answer"] or draft["source_ids"]:
                raise ValueError("abstention cannot include claims")
            result["error_code"] = "MODEL_ABSTAINED"
            return result
        if not draft["answer"].strip() or not draft["source_ids"]:
            raise ValueError("answer requires text and citations")
    except ModelError as error:
        result.update(status="FAILED", answer="", error_code=error.code)
        return result
    except (ValueError, TypeError, RecursionError):
        result.update(status="FAILED", answer="", error_code="MODEL_OUTPUT_INVALID")
        return result
    result.update(status=draft["status"], answer=draft["answer"], error_code="OK",
                  citations=[citation for identity in draft["source_ids"]
                             for citation in lookup[identity]["citations"]])
    return result
