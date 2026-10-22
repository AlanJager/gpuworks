"""Adapter for images that already contain Transformers and model dependencies."""

from __future__ import annotations

from typing import Any


class TransformersAdapter:
    def __init__(self, model: str, config: dict[str, Any]):
        from transformers import pipeline

        self.task = config.get("task", "text-generation")
        options = dict(config.get("load_kwargs", {}))
        self.call_kwargs = dict(config.get("call_kwargs", {}))
        self.pipe = pipeline(self.task, model=model, **options)

    def invoke(self, operation: str, payload: dict[str, Any], _files: dict[str, Any]) -> dict[str, Any]:
        if operation == "openai.chat":
            output = self.pipe(payload["messages"], **self.call_kwargs)
            content = generated_text(output)
            return {"content_type": "application/json", "body": {"object": "chat.completion", "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}]}}
        if operation == "openai.embeddings":
            output = self.pipe(payload["input"], **self.call_kwargs)
            vectors = output if isinstance(payload["input"], list) else [output]
            return {"content_type": "application/json", "body": {"object": "list", "data": [{"object": "embedding", "index": index, "embedding": flatten_vector(vector)} for index, vector in enumerate(vectors)]}}
        if operation == "openai.rerank":
            pairs = [{"text": payload["query"], "text_pair": document} for document in payload["documents"]]
            output = self.pipe(pairs, **self.call_kwargs)
            return {"content_type": "application/json", "body": {"results": [{"index": index, "relevance_score": score(value)} for index, value in enumerate(output)]}}
        raise ValueError(f"Transformers adapter does not implement {operation}")


def generated_text(output: Any) -> str:
    value = output[0] if isinstance(output, list) and output else output
    if isinstance(value, dict):
        value = value.get("generated_text", value.get("text", value))
    if isinstance(value, list) and value:
        tail = value[-1]
        if isinstance(tail, dict):
            value = tail.get("content", tail)
    return str(value)


def flatten_vector(value: Any) -> list[float]:
    while isinstance(value, list) and len(value) == 1 and isinstance(value[0], list):
        value = value[0]
    if not isinstance(value, list):
        raise TypeError("feature-extraction pipeline did not return a vector")
    return [float(item) for item in value]


def score(value: Any) -> float:
    if isinstance(value, dict):
        return float(value.get("score", 0))
    if isinstance(value, list) and value:
        return max(score(item) for item in value)
    return float(value)


def create(model: str, config: dict[str, Any]) -> TransformersAdapter:
    return TransformersAdapter(model, config)
