#!/usr/bin/env python3
"""Turn SGLang dummy-loader startup logs into the same shape as parse_logs.py.

SGLang prints sizes as "GB" but the numbers are binary GiB (verified by
reconstructing K bytes from #tokens x bytes-per-token), so they are treated as
GiB throughout.
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
    row = {"tag": os.path.basename(path)[:-4], "engine": "sglang"}
    if re.search(r"Traceback \(most recent call last\)|CUDA out of memory", text):
        row["error"] = "startup failure"
        m = re.search(r"^\S*(?:Error|Exception)[^\n]*$", text, re.M)
        if m:
            row["error_detail"] = m.group(0)[:200]

    row["model"] = re.search(r"model_path[=:] ?'?([^',\s]+)", text)
    row["model"] = row["model"].group(1) if row["model"] else None
    kv_dtype = re.search(r"KV Cache is allocated\. dtype: torch\.(\w+)", text)
    row["dtype"] = kv_dtype.group(1) if kv_dtype else None
    row["mem_fraction"] = num(r"mem_fraction_static[=:] ?([\d.]+)", text)
    row["context_len"] = num(r"context_length[=:] ?(\d+)", text, int)
    row["tp"] = num(r"tp_size[=:] ?(\d+)", text, int)

    # --- measured memory ---
    row["weights_gib"] = num(r"Load weight end\. elapsed=[\d.]+ s, type=\w+, avail mem=[\d.]+ GB, mem usage=([\d.]+) GB", text)
    row["avail_after_load_gib"] = num(r"Load weight end\..*?avail mem=([\d.]+) GB", text)
    row["kv_tokens"] = num(r"KV Cache is allocated\. dtype: torch\.\w+, #tokens: ([\d,]+)", text, int)
    row["k_gib"] = num(r"#tokens: [\d,]+, K size: ([\d.]+) GB", text)
    row["v_gib"] = num(r"#tokens: [\d,]+, K size: [\d.]+ GB, V size: ([\d.]+) GB", text)
    row["graph_gib"] = num(r"Capture cuda graph end\. Time elapsed: [\d.]+ s\. mem usage=([\d.]+) GB", text)
    row["max_running_requests"] = num(r"max_running_requests=([\d,]+)", text, int)

    if row.get("kv_tokens") and row.get("k_gib"):
        row["bytes_per_token_k"] = row["k_gib"] * GIB / row["kv_tokens"]
        row["bytes_per_token"] = 2 * row["bytes_per_token_k"]
    return row


def main():
    logdir = sys.argv[1] if len(sys.argv) > 1 else "/tmp/calib/logs-sglang"
    rows = [parse(os.path.join(logdir, f)) for f in sorted(os.listdir(logdir)) if f.endswith(".log")]
    out = sys.argv[2] if len(sys.argv) > 2 else "/tmp/calib/results-sglang.json"
    json.dump(rows, open(out, "w"), indent=1)
    print("wrote %s (%d rows)" % (out, len(rows)))
    cols = ["tag", "dtype", "mem_fraction", "context_len", "weights_gib", "graph_gib",
            "k_gib", "kv_tokens", "bytes_per_token", "error"]
    print("\t".join(cols))
    for r in rows:
        print("\t".join("" if r.get(c) is None else str(r.get(c)) for c in cols))


if __name__ == "__main__":
    main()
