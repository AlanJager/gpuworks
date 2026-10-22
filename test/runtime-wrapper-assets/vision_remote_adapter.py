"""Adapters for machine-validated remote-code vision model call shapes."""

from __future__ import annotations

import base64
import io
from typing import Any


def request_input(messages: list[dict[str, Any]]):
    from PIL import Image

    prompt_parts: list[str] = []
    image = None
    for message in messages:
        if message.get("role") != "user":
            continue
        content = message.get("content")
        if isinstance(content, str):
            prompt_parts.append(content)
            continue
        for item in content or []:
            if item.get("type") == "text":
                prompt_parts.append(str(item.get("text") or ""))
            elif item.get("type") == "image_url":
                value = item.get("image_url") or {}
                url = value.get("url") if isinstance(value, dict) else value
                if not str(url).startswith("data:image/") or ";base64," not in str(url):
                    raise ValueError("vision adapter accepts only inline base64 image_url data URLs")
                encoded = str(url).split(",", 1)[1]
                image = Image.open(io.BytesIO(base64.b64decode(encoded, validate=True))).convert("RGB")
    if image is None:
        raise ValueError("vision request requires one inline image_url")
    prompt = "\n".join(part.strip() for part in prompt_parts if part.strip())
    return prompt or "Describe the image.", image


def completion(content: str) -> dict[str, Any]:
    return {
        "content_type": "application/json",
        "body": {
            "object": "chat.completion",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": content}, "finish_reason": "stop"}],
        },
    }


class VisionRemoteAdapter:
    def __init__(self, model_path: str, config: dict[str, Any]):
        import torch

        self.torch = torch
        self.family = str(config.get("family") or "")
        self.max_new_tokens = int(config.get("max_new_tokens") or 128)
        if self.family == "ovis":
            from transformers import AutoModelForCausalLM

            self.model = AutoModelForCausalLM.from_pretrained(
                model_path, local_files_only=True, trust_remote_code=True,
                torch_dtype=torch.bfloat16, attn_implementation="eager", low_cpu_mem_usage=True,
            ).eval().cuda()
            self.processor = None
        elif self.family == "glm4v":
            import transformers
            from transformers import AutoTokenizer, Glm4vForConditionalGeneration, Glm4vImageProcessor, Glm4vProcessor

            tokenizer = AutoTokenizer.from_pretrained(model_path, local_files_only=True)
            image_processor = Glm4vImageProcessor.from_pretrained(model_path, local_files_only=True)
            self.processor = Glm4vProcessor(
                image_processor=image_processor, tokenizer=tokenizer,
                video_processor=object.__new__(transformers.BaseVideoProcessor), chat_template=tokenizer.chat_template,
            )
            self.model = Glm4vForConditionalGeneration.from_pretrained(
                model_path, local_files_only=True, torch_dtype=torch.bfloat16,
                attn_implementation="eager", low_cpu_mem_usage=True,
            ).eval().to("cuda:0")
        else:
            raise ValueError(f"unsupported vision remote-code family {self.family!r}")

    def invoke(self, operation: str, payload: dict[str, Any], _files: dict[str, Any]) -> dict[str, Any]:
        if operation != "openai.chat":
            raise ValueError(f"vision adapter does not implement {operation}")
        prompt, image = request_input(payload["messages"])
        if self.family == "ovis":
            messages = [{"role": "user", "content": [{"type": "image", "image": image}, {"type": "text", "text": prompt}]}]
            input_ids, pixel_values, grid_thws = self.model.preprocess_inputs(
                messages=messages, add_generation_prompt=True, enable_thinking=False, max_pixels=896 * 896,
            )
            input_ids = input_ids.cuda()
            pixel_values = pixel_values.cuda() if pixel_values is not None else None
            grid_thws = grid_thws.cuda() if grid_thws is not None else None
            with self.torch.inference_mode():
                output = self.model.generate(
                    inputs=input_ids, pixel_values=pixel_values, grid_thws=grid_thws,
                    enable_thinking=False, max_new_tokens=self.max_new_tokens,
                    do_sample=False, use_cache=False,
                )
            text = self.model.text_tokenizer.decode(output[0], skip_special_tokens=True).strip()
            return completion(text)

        messages = [{"role": "user", "content": [{"type": "image", "image": image}, {"type": "text", "text": prompt}]}]
        inputs = self.processor.apply_chat_template(
            messages, tokenize=True, add_generation_prompt=True, return_dict=True, return_tensors="pt",
        ).to("cuda:0")
        with self.torch.inference_mode():
            output = self.model.generate(**inputs, max_new_tokens=self.max_new_tokens, do_sample=False, use_cache=False)
        text = self.processor.decode(output[0][inputs["input_ids"].shape[1]:], skip_special_tokens=True).strip()
        return completion(text)


def create(model: str, config: dict[str, Any]) -> VisionRemoteAdapter:
    return VisionRemoteAdapter(model, config)
