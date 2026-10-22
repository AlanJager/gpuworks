#!/usr/bin/env python3
"""Ask vLLM what KV layout a model has, from its config.json alone.

The KV cache cost per token is a property of the model architecture, and vLLM
derives it from the HuggingFace config without touching weights or a device:
`ModelArchConfigConvertorBase.is_deepseek_mla`, `.get_head_size()` and
`.get_total_num_kv_heads()` are all pure config functions, and the KV cache
stores `num_kv_heads x head_size` elements per layer per token (MLA stores the
compressed latent in that same shape, which is why it is not doubled for K and
V).

That decouples two questions this harness had been treating as one:

  * what the KV layout is - answerable for any model whose config.json can be
    fetched, on a CPU, however large the model is;
  * what the engine reserves besides the weights - which does need a run with
    real weights resident, and is what the GPU sweeps measure.

    usage: kv_layout.py <config.json | dir> [...]

Run it inside the vLLM container so the answers come from vLLM's own code
rather than a re-implementation of it.
"""
import json
import os
import sys

# vllm.config must be imported first: the convertor module is imported from it,
# so importing the convertor directly trips a circular import.
import vllm.config  # noqa: F401
from vllm.transformers_utils.config import get_config
from vllm.transformers_utils import model_arch_config_convertor as mac

# The catalog stores one value per model at a fixed element width, because the
# deploy form picks the KV dtype and scales by it (DtypeSizeBytes is exact
# arithmetic). A config that publishes torch_dtype float32 - several embedding
# models do - would otherwise bake a doubled number into the catalog and make
# that scaling wrong.
BASE_ELEM = 2


def text_config(hf):
    """The stack that keeps the KV cache, mirroring vLLM's own selection."""
    getter = getattr(hf, "get_text_config", None)
    if getter is not None:
        try:
            return getter()
        except Exception:
            pass
    return getattr(hf, "text_config", None) or hf


# The token-indexed KV pool belongs to attention blocks.  Hybrid configs also
# list recurrent/feed-forward blocks (linear attention, Mamba, convolution,
# MLP, MoE) which do not add an attention KV entry.  The old generator treated
# every layer as vLLM KV and produced a 4x overestimate for Qwen3.5; mirror1
# vLLM 0.25.1 reports the same per-token footprint as SGLang for these models.
ATTENTION_LAYER_NAMES = {"attention", "full_attention", "sliding_attention", "global_attention"}


def attention_layer_count(text, layers):
    """Count token-indexed attention layers, or every layer for dense configs."""
    types = getattr(text, "layer_types", None)
    if not types:
        return layers
    counted = sum(1 for t in types if str(t).lower() in ATTENTION_LAYER_NAMES)
    # Unknown layer names are common in vendor configs.  If the list contains
    # no recognized attention entry, retain the conservative dense fallback
    # instead of silently publishing zero KV bytes/token.
    return counted or layers


def load_config(path):
    """Read a model config, preferring not to execute the repository's own code.

    A few catalog models ship custom modeling code, and transformers then refuses
    the config outright ("contains custom code which must be executed"). vLLM
    cannot load those without --trust-remote-code either, so a value for them can
    only come from running that code. The strict attempt goes first so nothing is
    executed for the models that do not need it, and TRUST_REMOTE_CODE=1 opts in
    to the fallback.
    """
    try:
        return get_config(path, trust_remote_code=False)
    except Exception:
        if os.environ.get("TRUST_REMOTE_CODE") != "1":
            raise
        return get_config(path, trust_remote_code=True)


def layout(path):
    # vLLM resolves a model directory or a repo id, not a bare config.json path.
    if not os.path.isdir(path):
        path = os.path.dirname(path)
    hf = load_config(path.rstrip("/"))
    text = text_config(hf)
    # Use the registered convertor for this architecture so architecture-specific
    # overrides apply; fall back to the base for unknown ones.
    conv_class = mac.ModelArchConfigConvertorBase
    arch = (getattr(hf, "architectures", None) or [""])[0]
    registry = getattr(mac, "MODEL_ARCH_CONFIG_CONVERTORS", {})
    if arch in registry:
        conv_class = registry[arch]
    conv = conv_class(hf, text)
    mlc = conv.is_deepseek_mla()
    head_size = conv.get_head_size()
    kv_heads = 1 if mlc else conv.get_total_num_kv_heads()
    layers = conv.get_num_hidden_layers()
    dtype = getattr(hf, "torch_dtype", None) or getattr(text, "torch_dtype", None) or "bfloat16"
    elem = BASE_ELEM
    # MLA caches one latent per layer per token; ordinary attention caches a K
    # and a V of the same shape.
    per_layer = kv_heads * head_size * (1 if mlc else 2) * elem
    kv_layers = attention_layer_count(text, layers)
    return {
        "model_type": getattr(text, "model_type", None),
        "layers": layers,
        "attention_layers": kv_layers,
        "mla": mlc,
        "head_size": head_size,
        "kv_heads": kv_heads,
        # Both engines use the attention layer count for the hybrid layouts
        # represented by the catalog.  Keep the two keys for compatibility
        # with existing catalog data; a future engine-specific divergence must
        # be backed by a runtime measurement before changing one independently.
        "bytes_per_token": kv_layers * per_layer,
        "bytes_per_token_sglang": kv_layers * per_layer,
        "dtype": str(dtype).split(".")[-1],
    }


def model_id_for(path):
    """Recover the catalog model id from the directory gen_catalog_kv.sh made."""
    return os.path.basename(path).replace("__", "/")


if "--json" in sys.argv:
    # vLLM logs to stdout, so the table goes to a file: anything on stdout is
    # interleaved with its INFO lines.
    # Emit the table the catalog is written from. A model that cannot be read
    # is absent rather than zero: absent means no value, which is a different
    # statement from a footprint of zero bytes.
    out_path = sys.argv[sys.argv.index("--json") + 1]
    table = {}
    for root in sys.argv[sys.argv.index("--json") + 2:]:
        for dirpath, _, files in os.walk(root):
            if "config.json" not in files:
                continue
            try:
                r = layout(dirpath)
            except Exception:
                continue
            if r["bytes_per_token"] > 0:
                table[model_id_for(dirpath)] = {
                    "vllm": r["bytes_per_token"],
                    "sglang": r["bytes_per_token_sglang"],
                }
    with open(out_path, "w") as fh:
        json.dump(table, fh, indent=1, sort_keys=True)
    sys.exit(0)

for path in sys.argv[1:]:
    name = os.path.basename(os.path.dirname(path)) or os.path.basename(path)
    try:
        r = layout(path)
    except Exception as exc:
        print("%-40s ERROR %s: %s" % (name, type(exc).__name__, str(exc)[:90]))
        continue
    print("%-40s type=%-14s layers=%-4s mla=%-6s kvh=%-4s head=%-5s -> %d bytes/token (%s)" % (
        name, r["model_type"], r["layers"], r["mla"], r["kv_heads"], r["head_size"],
        r["bytes_per_token"], r["dtype"]))
