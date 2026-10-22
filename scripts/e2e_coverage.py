#!/usr/bin/env python3
"""Build an offline business-coverage browser without running E2E tests.

Run: python3 scripts/e2e_coverage.py --reports /path/to/e2e/artifacts
The default report root and output live under gitignored secrets/e2e/.
Only allowlisted report metadata is exported, never requests, logs or secrets.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

sys.dont_write_bytecode = True

from e2e_coverage_inventory import inventory
from e2e_coverage_components import components
from e2e_coverage_provenance import fingerprints, source_file

STATUSES = {"PASS", "FAIL_PRODUCT", "FAIL_TEST", "BLOCKED_INFRA", "BLOCKED_PRODUCT",
            "MISSING", "MISSING_EVIDENCE", "NOT_RUN", "N/A", "RETIRED"}
HEX40 = re.compile(r"[0-9a-f]{40}\Z")
HEX64 = re.compile(r"[0-9a-f]{64}\Z")
MAX_REPORT_BYTES = 16 * 1024 * 1024


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def git(root, *args):
    return subprocess.check_output(["git", *args], cwd=root, text=True).strip()


def report_files(roots):
    """Do not descend into private databases, symlinks or dependency trees."""
    seen = set()
    for root in roots:
        if not root.exists():
            continue
        if root.is_file():
            candidates = [root]
        else:
            candidates = []
            for folder, directories, files in os.walk(root, followlinks=False):
                directories[:] = sorted(d for d in directories if d not in {
                    "private", ".git", "node_modules", "__pycache__"}
                    and not (Path(folder) / d).is_symlink())
                if "report.json" in files:
                    candidates.append(Path(folder) / "report.json")
        for path in candidates:
            if path.is_symlink() or path.resolve() in seen:
                continue
            seen.add(path.resolve())
            yield path


def summarize_report(report, contract, root, head, modified, mtime, binaries):
    """A projection, not an independent re-verification of product assertions."""
    status = report.get("status")
    status = status if isinstance(status, str) and status in STATUSES else "UNKNOWN"
    sha = report.get("source_sha")
    sha = sha if isinstance(sha, str) and HEX40.fullmatch(sha) else None
    locale = report.get("locale") if report.get("locale") in ("en", "zh") else "unspecified"
    run_id = report.get("run_id", report.get("runId", ""))
    run_id = run_id if isinstance(run_id, str) and re.fullmatch(r"[\w.:-]{1,100}", run_id) else "未标注"
    layers = sorted({e["layer"] for e in report.get("evidence", [])
                     if isinstance(e, dict) and isinstance(e.get("layer"), str)
                     and e["layer"] in contract.get("required_evidence", [])})
    reasons = fingerprints(report, contract, root)
    if report.get("working_tree") != "clean":
        reasons.append("运行时工作树未确认干净")
    if not sha:
        reasons.append("源码 SHA 缺失或无效")
    if set(contract.get("required_evidence", [])) - set(layers):
        reasons.append("缺少必需证据层")
    if not contract.get("required_evidence"):
        reasons.append("清单未声明必需证据层")
    if contract.get("conflict"):
        reasons.append("清单存在重复 ID 的冲突声明")
    if type(report.get("retries")) is not int or report["retries"] != 0:
        reasons.append("未确认零重试")
    binary_map = report.get("binary_sha256") if isinstance(report.get("binary_sha256"), dict) else {}
    binary = binary_map.get("server", report.get("binary_sha256"))
    agent_binary = report.get("agent_binary_sha256", binary_map.get("agent"))
    if not isinstance(binary, str) or not HEX64.fullmatch(binary):
        reasons.append("server 二进制摘要缺失")
    if "agent" in contract.get("lane", "") and (not isinstance(agent_binary, str) or not HEX64.fullmatch(agent_binary)):
        reasons.append("agent 二进制摘要缺失")
    binary_checked = None
    for field, expected in binaries.items():
        if expected:
            match = (binary if field == "binary_sha256" else agent_binary) == expected
            binary_checked = match if binary_checked is None else binary_checked and match
            if not match:
                reasons.append("与指定的本地二进制不匹配")
    if "remaining_resources" in report and report["remaining_resources"] != []:
        reasons.append("报告存在残留资源")
    if report.get("cleanup_failed_stages"):
        reasons.append("报告存在清理失败")
    if report.get("browser_close_ok") is not True:
        reasons.append("缺少浏览器关闭成功证明")
    if not any(report.get(key) is True for key in ("environment_cleanup_ok", "cleanup_ok")):
        reasons.append("缺少环境清理成功证明")
    if report.get("remaining_resources") != []:
        reasons.append("未确认残留资源为空")
    if "artifact_scan" in report and report["artifact_scan"] != "PASS":
        reasons.append("产物扫描未确认通过")
    if any(report.get(key) for key in ("artifact_errors", "artifact_error", "failure", "page_errors")):
        reasons.append("报告记录了运行或产物错误")
    stages = report.get("cleanup_steps", [])
    if not isinstance(stages, list) or any(not isinstance(stage, dict) or stage.get("ok") is not True for stage in stages):
        reasons.append("清理阶段未全部成功")
    for key in ("environment_cleanup_ok", "cleanup_ok", "browser_close_ok", "server_stopped", "agent_stopped"):
        if key in report and report[key] is not True:
            reasons.append("清理或关闭未确认成功")
    mutation = bool(report.get("product_mutation") or report.get("mutation"))
    if mutation:
        state = "mutation"
    elif sha and sha != head:
        state = "historical"
    elif modified:
        state = "uncommitted"
        reasons.append("查看的工作树有未提交改动，不能证明当前工作树通过")
    elif reasons:
        state = "incomplete"
    else:
        state = "current"
    # Ordering uses the declared UTC run ID where available, never status=PASS.
    stamp = re.match(r"^(\d{8}T\d{6}Z)", run_id)
    order = stamp.group(1) if stamp else datetime.fromtimestamp(mtime, timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    return {"run_id": run_id, "status": status, "source_sha": sha, "locale": locale,
            "state": state, "reasons": list(dict.fromkeys(reasons)), "layers": layers,
            "binary_checked": binary_checked, "order": order,
            "time_basis": "run_id" if stamp else "file_mtime"}


def attach_reports(data, roots, root, head, modified=False, binaries=None):
    by_id = {c["id"]: c for c in data["contracts"]}
    binaries = binaries or {}
    for contract in by_id.values():
        contract["runs"] = []
    read = matched = ignored = 0
    for path in report_files(roots):
        read += 1
        try:
            if path.stat().st_size > MAX_REPORT_BYTES:
                raise ValueError("oversized")
            report = json.loads(path.read_text())
            if not isinstance(report, dict) or not isinstance(report.get("evidence", []), list):
                raise ValueError("invalid report shape")
        except (OSError, ValueError, UnicodeError):
            data["warnings"].append(f"报告 #{read} 无法读取、格式无效或超过 16 MiB；未计为通过")
            continue
        # Only an explicit primary contract binds evidence. A selection list
        # alone cannot make one successful case cover all selected contracts.
        ident = report.get("contract", report.get("contract_id"))
        if not isinstance(ident, str) or ident not in by_id:
            ignored += 1
            continue
        contract = by_id[ident]
        contract["runs"].append(summarize_report(
            report, contract, root, head, modified, path.stat().st_mtime, binaries))
        matched += 1
    for contract in by_id.values():
        contract["runs"].sort(key=lambda r: (r["order"], r["status"].startswith("FAIL"), r["run_id"]), reverse=True)
    return {"files_read": read, "matched": matched, "unbound": ignored}


def render(data):
    # Both manifest text and untrusted report metadata stay inert, even in a
    # file:// page. The UI only inserts dynamic text using textContent.
    payload = json.dumps(data, ensure_ascii=False).replace("<", "\\u003c").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    template = Path(__file__).with_suffix(".html").read_text()
    return template.replace("<!--COVERAGE_DATA-->", payload)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--reports", type=Path, action="append", help="报告目录或单个 report.json，可重复；跳过 private 和符号链接")
    parser.add_argument("--output", type=Path, help="默认 secrets/e2e/coverage/index.html；不能覆盖仓库跟踪文件")
    parser.add_argument("--server-bin", type=Path, help="可选：额外核对报告中的 server 二进制摘要")
    parser.add_argument("--agent-bin", type=Path, help="可选：额外核对报告中的 agent 二进制摘要")
    args = parser.parse_args()
    root = args.root.resolve()
    data = inventory(root)
    data["components"] = components(root, data)
    head = git(root, "rev-parse", "HEAD")
    modified = bool(git(root, "status", "--porcelain", "--untracked-files=normal"))
    binaries = {field: digest(path) for field, path in (
        ("binary_sha256", args.server_bin), ("agent_binary_sha256", args.agent_bin)) if path}
    report_roots = args.reports or ([Path(os.environ["E2E_REPORT_DIR"])] if os.environ.get("E2E_REPORT_DIR") else [root / "secrets/e2e"])
    if (args.reports or os.environ.get("E2E_REPORT_DIR")) and any(not path.exists() for path in report_roots):
        parser.error("指定的报告来源不存在")
    data["reports"] = attach_reports(data, report_roots, root, head, modified, binaries)
    data["repository"] = {"head": head, "modified": modified,
                          "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds")}
    output = (args.output or (Path(os.environ["E2E_COVERAGE_OUTPUT"]) if os.environ.get("E2E_COVERAGE_OUTPUT") else root / "secrets/e2e/coverage/index.html")).resolve()
    if output.suffix.lower() != ".html" or output == Path(__file__).with_suffix(".html").resolve():
        parser.error("输出必须是独立的 .html 文件，不能覆盖查看器模板")
    if output.is_relative_to(root) and str(output.relative_to(root)) in git(root, "ls-files").splitlines():
        parser.error("输出文件不能覆盖仓库跟踪文件")
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(render(data), encoding="utf-8")
    output.chmod(0o600)
    print(f"E2E 业务覆盖查看页：{output.resolve()}")
    print(f"{len(data['contracts'])} 个登记条目；读取 {data['reports']['files_read']} 份报告。没有运行测试。")
    if data["warnings"]:
        print(f"{len(data['warnings'])} 条数据提示，详见页面。")


if __name__ == "__main__":
    main()
