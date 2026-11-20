#!/usr/bin/env python3
"""Launch the pinned pure-text target using its actual nested configuration."""
import json
import os
from pathlib import Path
import sys


def target_overrides(model):
    config = json.loads((Path(model) / "config.json").read_text())
    text = dict(config["text_config"])
    count = text["num_hidden_layers"]
    for name in ("layer_types", "indexer_types", "mlp_layer_types"):
        if len(text[name]) != count:
            raise ValueError(f"{name} does not describe all {count} target layers")
    text["model_type"] = config["model_type"]
    text["architectures"] = ["Glm5NextForCausalLM"]
    return text


def main():
    args = sys.argv[1:]
    if not args or args[0].startswith("-"):
        raise ValueError("the primary model path must be the first argument")
    if "--hf-overrides" in args:
        raise ValueError("this template derives HF overrides from the mounted model")
    overrides = target_overrides(args[0])
    print("GLM_TARGET_CONFIG " + json.dumps({
        "layers": overrides["num_hidden_layers"],
        "architecture": overrides["architectures"][0],
        "index_head_dim": overrides["index_head_dim"],
    }), flush=True)
    os.execvp("vllm", ["vllm", "serve", *args, "--hf-overrides", json.dumps(overrides)])


if __name__ == "__main__":
    main()
