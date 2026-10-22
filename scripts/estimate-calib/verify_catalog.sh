#!/usr/bin/env bash
# Compare the estimation facts a catalog entry declares against the facts the
# model's own published config.json yields.
#
# The estimator is only as right as the facts it is handed, and for a catalog
# model those facts are hand-written in model.yaml. This checks them
# independently of whether the KV formula itself is sound: fetch the published
# config.json, run the same parser over it, and diff the fields the KV math
# reads. A difference is not automatically a bug - a curator may legitimately
# fill a field the repository omits - but it is the thing to look at.
#
#   usage: verify_catalog.sh <targets.json>   (requires gocalc on PATH)
#
# targets.json maps model_id -> {cfg: <catalog model_config>, ms: <modelscope
# repo_id>, hf: <huggingface repo_id>}.
set -u
python3 - "${1:-/tmp/calib/targets.json}" "${2:-/tmp/calib/catalogcfg}" <<'PY'
import json, os, subprocess, sys, urllib.request

targets, work = json.load(open(sys.argv[1])), sys.argv[2]
os.makedirs(work, exist_ok=True)
GOCALC = os.environ.get("GOCALC", "gocalc")
FIELDS = ["num_hidden_layers", "hidden_size", "num_attention_heads", "num_key_value_heads",
          "head_dim", "effective_kv_heads", "effective_head_dim", "max_position_emb",
          "sliding_window", "layer_types", "is_mla", "kv_lora_rank", "qk_rope_head_dim"]


def fetch(repo):
    url = "https://modelscope.cn/api/v1/models/%s/repo?Revision=master&FilePath=config.json" % repo
    try:
        with urllib.request.urlopen(url, timeout=25) as r:
            return r.read() if r.status == 200 else None
    except Exception:
        return None


def evaluate(cfg, path):
    """Run the shipped estimator over a config and return its facts + verdict."""
    open(path, "w").write(json.dumps(cfg))
    # stdout/stderr explicitly: the harness runs on hosts whose python predates
    # capture_output.
    pipes = subprocess.Popen([GOCALC, "-config", path, "-weights-bytes", "1000000000",
                              "-gpu-bytes", "48318382080", "-util", "0.9", "-maxlen", "4096"],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    body, _ = pipes.communicate()
    if pipes.returncode != 0:
        return None, None
    d = json.loads(body.decode("utf-8"))
    return d["config_facts"], d["breakdown"]


def summarize(facts):
    return "layers=%s hidden=%s kvh=%s hdim=%s mla=%s sw=%s lt=%s" % (
        facts["num_hidden_layers"], facts["hidden_size"], facts["effective_kv_heads"],
        facts["effective_head_dim"], facts["is_mla"], facts["sliding_window"],
        "yes(%d)" % len(facts["layer_types"]) if facts["layer_types"] else "no")


mismatched = []
for mid, t in sorted(targets.items()):
    if not t.get("ms"):
        print("%-38s -- no modelscope source declared" % mid)
        continue
    raw = fetch(t["ms"])
    if raw is None:
        print("%-38s -- config.json unavailable at modelscope" % mid)
        continue
    realPath = os.path.join(work, mid.replace("/", "_") + ".json")
    open(realPath, "wb").write(raw)
    real, realVerdict = evaluate(json.load(open(realPath)), realPath)
    declared, declaredVerdict = evaluate(t["cfg"], os.path.join(work, "_declared.json"))
    if real is None or declared is None:
        print("%-38s -- unparseable" % mid)
        continue
    diffs = [(k, declared.get(k), real.get(k)) for k in FIELDS if declared.get(k) != real.get(k)]
    status = "OK  " if not diffs else "DIFF"
    print("%-38s %s grade=%-11s bpt=%-8d %s" % (
        mid, status, declaredVerdict["grade"], declaredVerdict["kv_bytes_per_token"], summarize(declared)))
    if diffs:
        mismatched.append(mid)
        print("%-38s      config.json says: %s" % ("", summarize(real)))
        for k, a, b in diffs:
            print("%-38s      %-22s catalog=%-12s config=%s" % ("", k, a, b))
        if realVerdict["kv_bytes_per_token"] != declaredVerdict["kv_bytes_per_token"]:
            print("%-38s      -> the estimate would change from %d to %d bytes/token" % (
                "", declaredVerdict["kv_bytes_per_token"], realVerdict["kv_bytes_per_token"]))

print("\n%d of %d entries disagree with their published config.json" % (len(mismatched), len(targets)))
PY
