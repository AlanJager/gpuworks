#!/usr/bin/env python3
"""The calibration table: shipped estimator vs dummy-loader measurement.

Three quantities are separated because they fail independently and need
different fixes:

  * bytes/token       - the KV arithmetic and the config extraction feeding it.
  * engine shortfall  - the gap between the GPU total the platform reports and
                        the total the engine can actually address. vLLM budgets
                        against cudaMemGetInfo()'s total, which is smaller than
                        nvidia-smi's by a driver-reserved amount, so this is
                        subtracted from the budget rather than added to the
                        reserve.
  * engine reserve    - non-weight memory the engine holds back (CUDA context,
                        activation peak, graph capture). Measured as
                        engine_total*util - weights - available_kv.

Every measured number comes from the engine's own startup output. Inputs are
results.json files produced by parse_logs.py / parse_sglang.py.
"""
import json
import os
import subprocess
import sys

GIB = float(1 << 30)
GOCALC = os.environ.get("GOCALC", "/tmp/gocalc")
CFGDIR = os.environ.get("CALIB_CFGDIR", "/tmp/calibcfg")

# tag prefix -> (config dir, nvidia-smi total bytes, engine-visible total GiB)
# The engine totals are the ones the runs themselves printed; the shortfall is
# the difference and is what the platform's budget silently over-counts.
MODELS = {
    "q3-0.6b": ("Qwen_Qwen3-0.6B-default", 48318382080, 44.29),
    "q25-7b": ("Qwen2.5-7B-Instruct", 48318382080, 44.29),
    "q3-8b": ("Qwen3-8B", 48318382080, 44.29),
    "llama31-8b": ("Llama-3.1-8B-Instruct", 48318382080, 44.29),
    "dsv2lite": ("DeepSeek-V2-Lite-Chat", 48318382080, 44.29),
    "gemma3-4b": ("gemma-3-4b-it", 48318382080, 44.29),
    "gemma3-12b": ("gemma-3-12b-it", 48318382080, 44.29),
    "gptoss-20b": ("gpt-oss-20b", 48318382080, 44.29),
    "mistral-v03": ("mistral-7b-instruct-v03", 48318382080, 44.29),
    "dbg-": ("Qwen_Qwen3-0.6B-default", 48318382080, 44.29),
    # device 1, RTX 2080 Ti
    "sg-q3-0.6b": ("Qwen_Qwen3-0.6B-default", 23622320128, 21.48),
    "sg-q25-7b": ("Qwen2.5-7B-Instruct", 23622320128, 21.48),
    "sg-gemma3-4b": ("gemma-3-4b-it", 23622320128, 21.48),
    # vast.ai A100 PCIE 80GB
    "a100-": (None, 85899345920, 79.25),
}
A100_MODELS = {
    "q3-0.6b": "Qwen_Qwen3-0.6B-default",
    "q25-7b": "Qwen2.5-7B-Instruct",
    "q3-8b": "Qwen3-8B",
    "gemma3-4b": "gemma-3-4b-it",
    "gptoss-20b": "gpt-oss-20b",
    "dsv2lite": "DeepSeek-V2-Lite-Chat",
    "q3-30b-a3b": "Qwen3-30B-A3B-Instruct-2507",
}


def lookup(tag):
    best = None
    for prefix in MODELS:
        if tag.startswith(prefix) and (best is None or len(prefix) > len(best)):
            best = prefix
    if not best:
        return None, None, None
    cfg, total, engine_total = MODELS[best]
    if cfg is None:  # a100-*: resolve from the suffix after the prefix
        rest = tag[len(best):]
        for key, name in sorted(A100_MODELS.items(), key=lambda kv: -len(kv[0])):
            if rest.startswith(key):
                cfg = name
                break
    return cfg, total, engine_total


def gocalc(cfgdir, weights_bytes, gpu_bytes, util, tp, maxlen, engine):
    p = subprocess.run(
        [GOCALC, "-config", os.path.join(CFGDIR, cfgdir),
         "-weights-bytes", str(int(weights_bytes)), "-gpu-bytes", str(gpu_bytes),
         "-util", str(util), "-tp", str(tp), "-maxlen", str(maxlen or 0),
         "-engine", engine],
        capture_output=True, text=True)
    return json.loads(p.stdout)["breakdown"] if p.returncode == 0 else None


def rows_from(paths):
    out = []
    for path in paths:
        if not os.path.exists(path):
            continue
        try:
            for r in json.load(open(path)):
                if r.get("kv_tokens"):
                    out.append(r)
        except (ValueError, KeyError):
            pass
    return out


def main():
    rows = rows_from(sys.argv[1:] or ["/tmp/calib/results.json",
                                      "/tmp/calib/results-sglang.json",
                                      "/tmp/a100-results.json"])
    print("%-28s %-7s %6s %7s | %10s %10s %8s | %11s %11s" % (
        "tag", "engine", "util", "mnbt", "meas_tok", "est_tok", "delta",
        "meas_bpt", "est_bpt"))
    print("-" * 122)
    for r in sorted(rows, key=lambda x: x["tag"]):
        cfg, total, engine_total = lookup(r["tag"])
        if not cfg:
            continue
        engine = r.get("engine", "vllm")
        util = r.get("mem_fraction") or r.get("gpu_mem_util") or 0.9
        b = gocalc(cfg, r["weights_gib"] * GIB, total, util, r.get("tp") or 1,
                   r.get("context_len") or r.get("max_model_len") or 0, engine)
        if not b:
            continue
        meas_tok, est_tok = r["kv_tokens"], b.get("estimated_kv_tokens", 0)
        if not est_tok:
            print("%-28s %-7s %6s %7s | %10d %10s   %s" % (
                r["tag"], engine, util, r.get("mnbt") or "-", meas_tok, "-",
                b.get("kv_estimate_reason") or b.get("grade")))
            continue
        print("%-28s %-7s %6s %7s | %10d %10d %7.1f%% | %11.0f %11.0f" % (
            r["tag"], engine, util, r.get("mnbt") or "-", meas_tok, est_tok,
            (est_tok / meas_tok - 1) * 100 if meas_tok else 0,
            r.get("bytes_per_token") or 0, b["kv_bytes_per_token"]))

    print("\n%-28s %-7s %6s %9s %9s %11s %11s %10s" % (
        "tag", "engine", "util", "weights", "meas_kv", "reserve", "shortfall", "budget"))
    print("-" * 116)
    for r in sorted(rows, key=lambda x: x["tag"]):
        cfg, total, engine_total = lookup(r["tag"])
        if not cfg:
            continue
        util = r.get("mem_fraction") or r.get("gpu_mem_util") or 0.9
        weights = r["weights_gib"]
        kv = r.get("available_kv_gib") or ((r.get("k_gib") or 0) + (r.get("v_gib") or 0))
        shortfall = (total / GIB - engine_total) * util
        engine_budget = engine_total * util
        reserve = engine_budget - weights - kv
        print("%-28s %-7s %6s %9.2f %9.2f %11.2f %11.2f %10.2f" % (
            r["tag"], r.get("engine", "vllm"), util, weights, kv, reserve, shortfall, engine_budget))


if __name__ == "__main__":
    main()
