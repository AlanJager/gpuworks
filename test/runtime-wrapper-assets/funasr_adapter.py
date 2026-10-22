"""Adapter for images that already contain FunASR."""

from __future__ import annotations

import os
import tempfile
from typing import Any


class FunASRAdapter:
    def __init__(self, model: str, config: dict[str, Any]):
        from funasr import AutoModel

        self.generate_kwargs = dict(config.get("generate_kwargs", {}))
        self.model = AutoModel(model=model, **dict(config.get("load_kwargs", {})))

    def invoke(self, operation: str, _payload: dict[str, Any], files: dict[str, Any]) -> dict[str, Any]:
        if operation != "openai.audio.transcribe":
            raise ValueError(f"FunASR adapter does not implement {operation}")
        upload = files["file"]
        suffix = os.path.splitext(upload.get("filename", "audio.wav"))[1] or ".wav"
        name = ""
        try:
            with tempfile.NamedTemporaryFile(prefix="gpuworks-audio-", suffix=suffix, delete=False) as temporary:
                name = temporary.name
                temporary.write(upload["content"])
            output = self.model.generate(input=name, **self.generate_kwargs)
        finally:
            if name:
                try:
                    os.remove(name)
                except FileNotFoundError:
                    pass
        return {"content_type": "application/json", "body": {"text": transcription_text(output)}}


def transcription_text(output: Any) -> str:
    value = output[0] if isinstance(output, list) and output else output
    if isinstance(value, dict):
        value = value.get("text", value.get("sentence", value))
    return str(value)


def create(model: str, config: dict[str, Any]) -> FunASRAdapter:
    return FunASRAdapter(model, config)
