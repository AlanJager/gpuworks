#!/usr/bin/env python3
"""Run the six first-batch contracts in en/zh, sequentially with no retries.

Run: make test-e2e-first-batch E2E_BATCH_DIR=secrets/e2e/first-batch-final
Requires exclusive use of the explicitly selected private Docker daemon, cached
images, Chrome/Playwright, clean committed source and freshly built binaries.
Each child keeps its own report/cleanup evidence. batch-report.json is an index,
never evidence for another contract. A failure leaves later cases NOT_RUN.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))
from e2e_coverage import summarize_report

# Stable order; Docker cases run serially, including their cleanup.
CASES = (
    ("health", "deployment_health.py", "deployment-health.json", ()),
    ("diagnostics", "instance_diagnostics.py", "instance-diagnostics.json", ()),
    ("terminal", "container_terminal.py", "container-terminal.json", ()),
    ("huggingface", "model_hub_registration.py", "model-hub-huggingface.json", ("--case", "huggingface")),
    ("modelscope", "model_hub_registration.py", "model-hub-modelscope.json", ("--case", "modelscope")),
    ("quick", "model_hub_registration.py", "model-hub-quick.json", ("--case", "quick")),
)


def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT, text=True).strip()


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def checked(condition, message):
    if not condition:
        raise AssertionError(message)


def binary_identity(path, product, source_sha):
    line = subprocess.check_output([str(path), "--version"], text=True, timeout=10).strip()
    match = re.fullmatch(re.escape(product) + r" ([^\n]+) \(([0-9a-f]{7,40}), [^\n]+\)", line)
    checked(match and source_sha.startswith(match[2]) and "dirty" not in match[1],
            product + " build metadata must match clean current source")
    return line


def validate(report, manifest, source_sha, target_sha, binaries, locale, run_ids):
    checked(isinstance(report, dict), "child report must be an object")
    checked(report.get("contract") == manifest["contract_id"], "child contract differs")
    checked(report.get("locale") == locale, "child locale differs")
    checked(report.get("target_sha") == target_sha, "child target differs")
    checked(report.get("source_sha") == source_sha, "child source differs")
    run_id = report.get("run_id")
    checked(isinstance(run_id, str) and run_id and run_id not in run_ids, "missing or reused child run ID")
    contract = {**manifest, "id": manifest["contract_id"],
                "manifest": manifest["manifest"], "details": manifest}
    applicable_binaries = {"binary_sha256": binaries["binary_sha256"]}
    if "agent" in manifest.get("lane", ""):
        applicable_binaries["agent_binary_sha256"] = binaries["agent_binary_sha256"]
    summary = summarize_report(report, contract, ROOT, source_sha, False, 0, applicable_binaries)
    checked(summary["state"] == "current" and summary["status"] == "PASS",
            "child is not current ordinary PASS: " + "; ".join(summary["reasons"]))
    checked(report.get("case_completed") is True and report.get("artifact_scan") == "PASS",
            "child completion/scan not confirmed")
    run_ids.add(run_id)
    return run_id


def run(args, *, cases=None, kind="first-batch-index"):
    cases = CASES if cases is None else cases
    os.umask(0o077)
    directory = args.artifact_dir.resolve()
    # Never overwrite an earlier report or consume a stale child report.
    directory.mkdir(parents=True, exist_ok=False)
    (directory / "private").mkdir()
    report = {"kind": kind, "status": "NOT_RUN", "retries": 0,
              "started_at": datetime.now(timezone.utc).isoformat(), "cases": [
                  {"case": name, "locale": locale, "status": "NOT_RUN",
                   "report": f"{name}-{locale}/report.json"}
                  for name, *_ in cases for locale in ("en", "zh")]}

    def save():
        temporary = directory / "batch-report.tmp"
        temporary.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
        temporary.replace(directory / "batch-report.json")

    save()
    try:
        checked(not git("status", "--porcelain"), "committed clean source required")
        source_sha, target_sha = git("rev-parse", "HEAD"), git("rev-parse", "wei/master")
        subprocess.run(["git", "merge-base", "--is-ancestor", target_sha, source_sha], cwd=ROOT, check=True)
        binaries = {"binary_sha256": digest(args.server_bin), "agent_binary_sha256": digest(args.agent_bin)}
        versions = {"server": binary_identity(args.server_bin, "gpuworks", source_sha),
                    "agent": binary_identity(args.agent_bin, "gpuworks-agent", source_sha)}
        report.update(source_sha=source_sha, target_sha=target_sha, binaries=binaries, build_versions=versions)
        manifests = {}
        for name, driver, manifest_file, extra in cases:
            checked((ROOT / "test/e2e" / driver).is_file(), "missing driver: " + driver)
            relative = "test/e2e/coverage/" + manifest_file
            manifests[name] = {**json.loads((ROOT / relative).read_text()), "manifest": relative}
        seen = set()
        for row in report["cases"]:
            name, locale = row["case"], row["locale"]
            _, driver, _, extra = next(case for case in cases if case[0] == name)
            checked(git("rev-parse", "HEAD") == source_sha and not git("status", "--porcelain"),
                    "source changed during batch")
            checked(git("rev-parse", "wei/master") == target_sha, "target changed during batch")
            checked(binaries == {"binary_sha256": digest(args.server_bin),
                                 "agent_binary_sha256": digest(args.agent_bin)}, "binary changed during batch")
            row["status"] = "RUNNING"
            save()
            child_dir = directory / f"{name}-{locale}"
            command = [sys.executable, "-B", str(ROOT / "test/e2e" / driver), *extra,
                       "--server-bin", str(args.server_bin), "--agent-bin", str(args.agent_bin),
                       "--artifact-dir", str(child_dir), "--locale", locale]
            # Logs can include raw startup failures; retain privately, never copy to index.
            with (directory / "private" / f"{name}-{locale}.log").open("xb") as log:
                result = subprocess.run(command, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
            row["exit_code"] = result.returncode
            try:
                checked(result.returncode == 0, "child exited nonzero; inspect its report/private log")
                child = json.loads((child_dir / "report.json").read_text())
                row["run_id"] = validate(child, manifests[name], source_sha, target_sha, binaries, locale, seen)
                row["status"] = "PASS"
            except Exception:
                row["status"] = "FAIL"
                raise
            save()
        checked(git("rev-parse", "HEAD") == source_sha and not git("status", "--porcelain"),
                "source changed before completion")
        checked(git("rev-parse", "wei/master") == target_sha and binaries == {
            "binary_sha256": digest(args.server_bin), "agent_binary_sha256": digest(args.agent_bin)},
            "target or binary changed before completion")
        report["status"] = "PASS"
    except (Exception, KeyboardInterrupt):
        # Details remain in private child artifacts; no secret-bearing exceptions in index.
        report["status"] = "FAIL"
        for row in report["cases"]:
            if row["status"] == "RUNNING":
                row["status"] = "FAIL"
    finally:
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        save()
    print(json.dumps({"status": report["status"], "index": str(directory / "batch-report.json")}))
    return 0 if report["status"] == "PASS" else 1


def main(*, cases=None, kind="first-batch-index", description=None):
    parser = argparse.ArgumentParser(description=description or __doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path, required=True)
    args = parser.parse_args()
    args.server_bin, args.agent_bin = args.server_bin.resolve(), args.agent_bin.resolve()
    return run(args, cases=cases, kind=kind)


if __name__ == "__main__":
    raise SystemExit(main())
