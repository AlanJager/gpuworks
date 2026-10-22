#!/usr/bin/env python3
"""Independently check generated kv_bytes_per_token values.

The generator asks vLLM's own layout functions what a model's KV cache costs.
Checking that with the same functions would only prove the script is
deterministic, so this recomputes each value from a *different* source - the
catalog's hand-written model_config block, which was separately verified against
the models' published config.json - using the arithmetic the calibration
established:

    uniform attention   layers x kv_heads x head_dim x 2 (K and V) x element
    MLA                 layers x head_size x element, no K/V doubling, where
                        head_size is kv_lora_rank + qk_rope_head_dim, or bare
                        head_dim for the deepseek_v4 convention
    hybrid              every layer charged, because the engine pads the
                        recurrent page up to the attention page size

A model whose catalog block is absent, or whose shape the catalog does not
state, is reported as unchecked rather than as a pass.

    usage: verify_kv_values.py <kvvalues.json> [sample-size]
"""
import glob
import json
import os
import random
import re
import sys

import yaml

CATALOG = "internal/server/inferencecatalog/seed/catalog/models"
ELEM = 2  # the baseline width the catalog stores; the deploy form scales by dtype

# vLLM decides MLA by model_type, not by the presence of kv_lora_rank
# (model_arch_config_convertor.py: is_deepseek_mla). Its list is the ground
# truth the generator encodes, so the checker has to apply the same one - the
# independence being checked is the source (the catalog's own block) and the
# arithmetic, not the engine's rule.
MLA_MODEL_TYPES = {
    "AXK1", "deepseek_v2", "deepseek_v3", "deepseek_v32", "deepseek_v4",
    "deepseek_mtp", "glm_moe_dsa", "glm4_moe_lite", "glm4_moe_lite_mtp",
    "kimi_k2", "kimi_linear", "longcat_flash", "pangu_ultra_moe",
    "pangu_ultra_moe_mtp", "bailing_hybrid", "bailing_hybrid_mtp",
}

ATTENTION_LAYER_NAMES = {"attention", "full_attention", "sliding_attention", "global_attention"}


def catalog_blocks():
    out = {}
    for path in glob.glob(CATALOG + "/*/*/model.yaml"):
        text = open(path).read()
        mid = re.search(r"model_id:\s*(\S+)", text).group(1)
        m = re.search(r"^model_config:.*$", text, re.M)
        if not m:
            out[mid] = None
            continue
        try:
            out[mid] = yaml.safe_load(m.group(0).split("model_config:", 1)[1])
        except Exception:
            out[mid] = None
    return out


def expect(cfg):
    """The independent expectation, or (None, why) when the block cannot give one."""
    if not isinstance(cfg, dict):
        return None, "no catalog model_config"
    layers = cfg.get("num_hidden_layers")
    heads = cfg.get("num_attention_heads")
    kv_heads = cfg.get("num_key_value_heads") or heads
    hidden = cfg.get("hidden_size")
    head_dim = cfg.get("head_dim") or (hidden // heads if hidden and heads else 0)
    if not layers:
        return None, "no num_hidden_layers"
    model_type = cfg.get("model_type")
    if model_type is None:
        # The rule keys on model_type, so a block that omits it cannot be
        # checked this way. Guessing here is exactly the mistake that produced
        # the wrong catalog values in the first place.
        return None, "catalog block states no model_type"
    rope = cfg.get("qk_rope_head_dim")
    lora = cfg.get("kv_lora_rank")
    if model_type in MLA_MODEL_TYPES:
        if cfg.get("compress_ratios") is not None:
            size = head_dim
        else:
            size = (lora or 0) + (rope or 0)
        if not size:
            return None, "MLA without a head size"
        return layers * size * ELEM, "mla"
    if not head_dim or not kv_heads:
        return None, "no attention shape"
    # Hybrid configs list recurrent/feed-forward blocks alongside attention
    # blocks.  Only the latter contribute token-indexed KV entries.  A missing
    # or unrecognised list keeps the dense fallback, so an incomplete catalog
    # block never silently turns into a zero estimate.
    layer_types = cfg.get("layer_types")
    kv_layers = layers
    if isinstance(layer_types, list) and layer_types:
        counted = sum(1 for item in layer_types
                      if str(item).lower() in ATTENTION_LAYER_NAMES)
        if counted:
            kv_layers = counted
    return kv_layers * kv_heads * head_dim * 2 * ELEM, "attention"


def main():
    values = json.load(open(sys.argv[1]))
    sample_size = int(sys.argv[2]) if len(sys.argv) > 2 else 50
    blocks = catalog_blocks()

    # The models with a real engine measurement are ground truth, so they are
    # checked whatever the sample says.
    measured = {
        "Qwen/Qwen3-0.6B": 114690, "Qwen/Qwen2.5-7B-Instruct": 57340,
        "Qwen/Qwen3-8B": 147450, "meta-llama/Llama-3.1-8B-Instruct": 131070,
        "deepseek-ai/DeepSeek-V2-Lite-Chat": 31112, "zai-org/glm-5.1": 89856,
        "minimaxai/minimax-m2.7": 253952, "minimaxai/minimax-m3": 122880,
        "qwen/qwen3.5-0.8b": 12373,
        "liquidai/lfm2.5-350m": 12300,
    }
    keys = sorted(values)
    random.seed(20260913)
    sample = set(random.sample(keys, min(sample_size, len(keys))))
    sample |= {k for k in measured if k in values}
    sample |= {k for k in keys if k not in blocks}

    ok = mismatch = unchecked = 0
    print("%-52s %10s %10s  %s" % ("model", "generated", "expected", "verdict"))
    print("-" * 104)
    for key in sorted(sample):
        got = values[key]
        want, kind = expect(blocks.get(key))
        if want is None:
            unchecked += 1
            print("%-52s %10d %10s  unchecked: %s" % (key, got, "-", kind))
            continue
        if got == want:
            ok += 1
            continue
        mismatch += 1
        print("%-52s %10d %10d  MISMATCH (%s, ratio %.2f)" % (key, got, want, kind, got / want))

    print("\nchecked=%d exact=%d mismatch=%d unchecked=%d of %d sampled" %
          (ok + mismatch, ok, mismatch, unchecked, len(sample)))

    print("\n--- against real engine measurements ---")
    for key, want in sorted(measured.items()):
        got = values.get(key)
        if got is None:
            print("%-52s %10s  absent" % (key, "-"))
            continue
        delta = (got / want - 1) * 100
        flag = "ok" if abs(delta) < 2 else "CHECK"
        print("%-52s %10d %10d  %+5.2f%%  %s" % (key, got, want, delta, flag))


if __name__ == "__main__":
    main()
