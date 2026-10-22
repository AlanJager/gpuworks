#!/usr/bin/env python3
"""Turn vLLM dummy-loader startup logs into the calibration table.

Every number is read out of vLLM's own memory-profiling output; nothing is
inferred from the arguments we passed, so a run that failed to profile shows up
as a row with an error instead of silently contributing wrong data.
"""
import json
import os
import re
import sys

GIB = float(1 << 30)


def num(pat, text, cast=float):
    m = re.search(pat, text)
    return cast(m.group(1).replace(",", "")) if m else None


def parse(path):
    text = open(path, encoding="utf-8", errors="replace").read()
    row = {"tag": os.path.basename(path)[:-4]}
    if re.search(r"Traceback \(most recent call last\)|EngineCore failed|ERROR .*Engine core", text):
        row["error"] = "startup failure"
        first = re.search(r"^\S*(?:Error|Exception):.*$", text, re.M)
        if first:
            row["error_detail"] = first.group(0)[:200]

    # The engine config line carries the effective knobs, including defaults we
    # never passed (dtype, kv_cache_dtype, tp, cudagraph mode).
    row["model"] = re.search(r"model='([^']+)'", text).group(1) if "model='" in text else None
    row["dtype"] = re.search(r"^.*?dtype=torch\.(\w+)", text, re.M).group(1) if re.search(r"dtype=torch\.(\w+)", text) else None
    row["kv_cache_dtype"] = re.search(r"kv_cache_dtype=(\w+)", text).group(1) if re.search(r"kv_cache_dtype=(\w+)", text) else None
    row["tp"] = num(r"tensor_parallel_size=(\d+)", text, int)
    row["max_model_len"] = num(r"max_seq_len=(\d+)", text, int)
    row["gpu_mem_util"] = num(r"'gpu_memory_utilization': ([\d.]+)", text)
    row["enforce_eager"] = bool(re.search(r"enforce_eager=True", text))
    row["mnbt"] = num(r"max_num_batched_tokens=(\d+)", text, int)
    row["cudagraph_mode"] = re.search(r"cudagraph_mode': <CUDAGraphMode\.(\w+)", text)
    row["cudagraph_mode"] = row["cudagraph_mode"].group(1) if row["cudagraph_mode"] else None
    row["attn_backend"] = re.search(r"Using (\w+) attention backend", text)
    row["attn_backend"] = row["attn_backend"].group(1) if row["attn_backend"] else None

    # --- measured memory, all from vLLM's profiling ---
    row["weights_gib"] = num(r"Model loading took ([\d.]+) GiB", text)
    row["available_kv_gib"] = num(r"Available KV cache memory: ([\d.]+) GiB", text)
    row["kv_tokens"] = num(r"GPU KV cache size: ([\d,]+) tokens", text, int)
    row["max_concurrency"] = num(r"Maximum concurrency for [\d,]+ tokens per request: ([\d.]+)x", text)
    row["cg_equiv_util"] = num(r"equivalent to --gpu-memory-utilization=([\d.]+) without", text)
    row["cg_suggested_util"] = num(r"increase\s+--gpu-memory-utilization to ([\d.]+)", text)

    # GPU total comes from the profiling report vLLM prints alongside the KV line.
    row["total_gib"] = num(r"total_gpu_memory=([\d.]+)", text) or num(
        r"\(\s*([\d.]+)\s*GiB total\)", text)

    if row.get("kv_tokens") and row.get("available_kv_gib"):
        row["bytes_per_token"] = row["available_kv_gib"] * GIB / row["kv_tokens"]
    return row


def main():
    logdir = sys.argv[1] if len(sys.argv) > 1 else "/tmp/calib/logs"
    rows = [parse(os.path.join(logdir, f)) for f in sorted(os.listdir(logdir)) if f.endswith(".log")]
    out = sys.argv[2] if len(sys.argv) > 2 else "/tmp/calib/results.json"
    json.dump(rows, open(out, "w"), indent=1)
    print("wrote %s (%d rows)" % (out, len(rows)))
    cols = ["tag", "dtype", "kv_cache_dtype", "gpu_mem_util", "max_model_len", "mnbt",
            "enforce_eager", "cudagraph_mode", "weights_gib", "available_kv_gib",
            "kv_tokens", "bytes_per_token", "cg_equiv_util", "error"]
    print("\t".join(cols))
    for r in rows:
        print("\t".join("" if r.get(c) is None else str(r.get(c)) for c in cols))


if __name__ == "__main__":
    main()
