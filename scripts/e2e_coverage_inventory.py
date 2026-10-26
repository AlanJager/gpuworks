#!/usr/bin/env python3
"""Derive an E2E contract inventory from existing manifests and product navigation.

Registration and declared assertions are not execution results or a coverage rate.
Only canonical repository files are read; no manifests or test code are executed.
"""
from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path


_MISSING = re.compile(r"\b(?:MISSING(?:_EVIDENCE)?|NOT_COVERED|FAIL_TEST)\b", re.I)
_ABSENT = re.compile(r"^(?:N/A|NOT[ _]APPLICABLE)\b", re.I)
_SKIP_CLAIMS = {
    "additional_case", "additional_cases", "related_contract", "other_field_contract",
    "field_contract_reference", "covered_by_additional_cases", "product_contracts",
    "runtime_evidence", "execution_evidence", "failure_table", "case_bindings",
}
_GAP_KEYS = {"missing", "gaps", "limitations", "separate_flow_not_covered", "evidence_limits"}
_COVER_KEYS = {"covered", "covered_in_this_scenario"}
_ALIASES = {
    "api-tokens": "apikeys", "inference-key": "apikeys", "platform-token": "apikeys",
    "provider": "providers", "node": "nodes", "model": "models",
    "image-catalog": "images", "image-runtime": "images", "network": "networks",
    "deployment": "deployments", "templates": "inference-templates",
    "benchmark": "evaluations", "session": "login", "mcp": "settings",
    "playground/chat": "playground",
}


def _safe_file(root: Path, relative: str) -> Path | None:
    """Reject paths that could read outside the selected source checkout."""
    if not isinstance(relative, str) or not relative or Path(relative).is_absolute():
        return None
    candidate = (root / relative).resolve()
    if not candidate.is_relative_to(root) or not candidate.is_file():
        return None
    return candidate


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


# The menu moved from App.tsx into its own module; a tree from before the
# move still has it in App.tsx.
NAVIGATION_SOURCES = ("ui/src/navigation.tsx", "ui/src/App.tsx")


def _modules(root: Path, warnings: list[str]) -> list[dict]:
    source_name = next((name for name in NAVIGATION_SOURCES if _safe_file(root, name)), NAVIGATION_SOURCES[0])
    app = _safe_file(root, source_name)
    locale = _safe_file(root, "ui/src/i18n/locales/zh/common.json")
    navigation = {}
    if locale:
        try:
            navigation = json.loads(locale.read_text()).get("navigation", {})
        except (OSError, ValueError, AttributeError) as error:
            warnings.append(f"Cannot read navigation translations: {error}")
    modules = []
    if app:
        source = app.read_text()
        block = re.search(r"\bconst\s+navGroups\s*=\s*\[(.*?)\n\s*\];", source, re.S)
        if block:
            for path, key in re.findall(
                r'\{\s*key:\s*["\']([^"\']+)["\'].*?labelKey:\s*["\']navigation\.([^"\']+)["\']',
                block.group(1),
            ):
                module_id = path.strip("/").split("/")[0]
                name = navigation.get(key, module_id)
                if not any(item["id"] == module_id for item in modules):
                    modules.append({"id": module_id, "name": name, "paths": [path]})
                else:
                    next(item for item in modules if item["id"] == module_id)["paths"].append(path)
        if not modules:
            warnings.append(f"Cannot derive navigation modules from {source_name} navGroups")
    else:
        warnings.append(f"Navigation source {source_name} is absent")
    for module_id, name, path in [
        ("login", "登录与会话", "/login"),
        ("terminal", "终端", "/terminal"),
        ("assistant", "AI 助手", "/assistant"),
        ("unclassified", "未分类", ""),
    ]:
        if not any(item["id"] == module_id for item in modules):
            modules.append({"id": module_id, "name": name, "paths": [path] if path else []})
    return modules


def _pointer(path: str, value: str | int) -> str:
    return path + "/" + str(value).replace("~", "~0").replace("/", "~1")


def _leaf_texts(value, path: str):
    if isinstance(value, dict):
        for key, child in value.items():
            yield from _leaf_texts(child, _pointer(path, key))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            yield from _leaf_texts(child, _pointer(path, index))
    elif value is not None:
        yield {"path": path, "text": str(value)}


def _claims(data: dict) -> tuple[list[dict], list[dict]]:
    covered, missing = [], []

    def append(target, value, path):
        target.extend(_leaf_texts(value, path))

    def walk(value, path="", section=""):
        if isinstance(value, dict):
            identity = value.get("field_id", value.get("id", value.get("item")))
            if isinstance(identity, (str, int)):
                path += " [" + str(identity) + "]"
            status = value.get("status", "")
            if isinstance(status, str):
                reason = value.get("reason", value.get("assertion", status))
                if status.lower() == "covered":
                    append(covered, reason, _pointer(path, "status"))
                elif _MISSING.search(status):
                    append(missing, reason, _pointer(path, "status"))
            for key, child in value.items():
                if key in _SKIP_CLAIMS or key in {"cases", "entries", "status", "reason"}:
                    continue
                child_path = _pointer(path, key)
                if key in _COVER_KEYS:
                    append(covered, child, child_path)
                elif key in _GAP_KEYS:
                    append(missing, child, child_path)
                elif isinstance(child, str) and section in {"crud", "checklist_items", "partial_checklist"}:
                    if _ABSENT.search(child):
                        continue
                    if _MISSING.search(child):
                        append(missing, child, child_path)
                        if child.upper().startswith("PARTIAL:"):
                            append(covered, child, child_path)
                    else:
                        append(covered, child, child_path)
                else:
                    walk(child, child_path, key if key in {"crud", "checklist_items", "partial_checklist"} else section)
        elif isinstance(value, list):
            for index, child in enumerate(value):
                walk(child, _pointer(path, index), section)

    walk(data)
    return covered, missing


def _entries(data: dict, manifest: str, warnings: list[str]):
    """Expand only top-level execution declarations, never cross-reference objects."""
    if "entries" in data:
        entries = data["entries"]
        if not isinstance(entries, list):
            warnings.append(f"{manifest}: entries must be a list")
            return
        for index, entry in enumerate(entries):
            if not isinstance(entry, dict) or not isinstance(entry.get("entry_id"), str):
                warnings.append(f"{manifest}: unsupported legacy entry {index}")
                continue
            yield {**{k: v for k, v in data.items() if k != "entries"}, **entry}, {"entry_id": entry["entry_id"]}, "legacy"
        return
    if "cases" in data:
        cases = data["cases"]
        if not isinstance(cases, (list, dict)):
            warnings.append(f"{manifest}: cases must be an array or object")
            return
        parent = {k: v for k, v in data.items() if k != "cases"}
        for key, case in (cases.items() if isinstance(cases, dict) else enumerate(cases)):
            if not isinstance(case, dict):
                warnings.append(f"{manifest}: unsupported case {key}")
                continue
            contract_id = case.get("contract_id", case.get("id"))
            if not isinstance(contract_id, str) or not contract_id:
                warnings.append(f"{manifest}: case {key} lacks contract_id/id; parent umbrella is not a leaf")
                continue
            effective = {**parent, **case, "contract_id": contract_id}
            selection = {"case": case.get("case", case.get("case_id", case.get("selection", str(key))))}
            inherited = {name: parent[name] for name in parent if name in case and name in
                         {"fields", "crud", "checklist", "checklist_items", "component_checklist",
                          "partial_checklist", "covered", "missing", "gaps", "limitations"}}
            if inherited:
                effective["inherited_declarations"] = inherited
            if "parameters" in case:
                selection["parameters"] = case["parameters"]
            yield effective, selection, "runner" if "offline-runner" in str(data.get("lane", "")) else "business"
        return
    if "contract_ids" in data:
        ids = data["contract_ids"]
        parameters = data.get("parameters", [])
        if not isinstance(ids, list) or not isinstance(parameters, list):
            warnings.append(f"{manifest}: contract_ids/parameters must be arrays")
            return
        if len(ids) != len(parameters):
            warnings.append(f"{manifest}: contract_ids/parameters length mismatch")
        for index, contract_id in enumerate(ids):
            if not isinstance(contract_id, str) or not contract_id:
                warnings.append(f"{manifest}: invalid contract_ids[{index}]")
                continue
            selection = {"parameter": parameters[index]} if index < len(parameters) else {}
            yield {**data, "contract_id": contract_id}, selection, "business"
        return
    if isinstance(data.get("contract_id"), str) and data["contract_id"]:
        yield data, {}, "business"


def _module(data: dict, contract_id: str, known: set[str]) -> str:
    candidates = [str(data.get("page_id", "")).strip("/")]
    parts = contract_id.split("/")
    if len(parts) > 1:
        candidates.append(parts[1])
    for candidate in candidates:
        candidate = _ALIASES.get(candidate, candidate)
        if candidate in known:
            return candidate
    return "unclassified"


def _fallback(manifest: str, message: str, digest: str = "") -> dict:
    return {
        "id": "unsupported/" + Path(manifest).stem, "name": Path(manifest).stem,
        "module": "unclassified", "priority": "", "lane": "", "kind": "legacy",
        "manifest": manifest, "test_file": "", "test_function": "", "command": "",
        "selection": {}, "required_evidence": [], "covered": [],
        "missing": [{"path": "/", "text": message}], "details": {"inventory_error": message},
        "manifest_sha256": digest, "test_sha256": "",
    }


def inventory(root: Path) -> dict:
    """Return derived modules, unique execution contracts, and explicit source warnings."""
    root = root.resolve()
    warnings: list[str] = []
    modules = _modules(root, warnings)
    known = {module["id"] for module in modules}
    contracts: dict[str, dict] = {}
    manifest_files = []
    directory = root / "test/e2e/coverage"
    for manifest_path in sorted(directory.glob("*.json")):
        relative = manifest_path.relative_to(root).as_posix()
        manifest_files.append(relative)
        safe_path = _safe_file(root, relative)
        digest = _sha(safe_path) if safe_path else ""
        try:
            if not safe_path:
                raise ValueError("manifest is not a repository-contained regular file")
            data = json.loads(safe_path.read_text())
            if not isinstance(data, dict):
                raise ValueError("manifest root must be an object")
        except (OSError, ValueError) as error:
            message = f"{relative}: cannot parse manifest: {error}"
            warnings.append(message)
            fallback = _fallback(relative, message, digest)
            contracts[fallback["id"]] = fallback
            continue
        entries = list(_entries(data, relative, warnings))
        if not entries:
            message = f"{relative}: no supported execution contract declarations"
            warnings.append(message)
            fallback = _fallback(relative, message, digest)
            fallback["details"] = data
            contracts[fallback["id"]] = fallback
        for effective, selection, kind in entries:
            contract_id = effective.get("entry_id") if kind == "legacy" else effective["contract_id"]
            module = _module(effective, contract_id, known)
            if module == "unclassified" and kind == "business":
                warnings.append(f"{relative}: no navigation module mapping for {contract_id}")
            test_file = effective.get("test_file", "")
            if not isinstance(test_file, str):
                warnings.append(f"{relative}: invalid test_file for {contract_id}")
                test_file = ""
            entry_file = _safe_file(root, test_file)
            if not entry_file:
                warnings.append(f"{relative}: missing or unsafe test file {test_file!r} for {contract_id}")
            covered, missing = _claims(effective)
            entry = {
                "id": contract_id, "name": effective.get("name", effective.get("resource", contract_id)),
                "module": module, "priority": effective.get("priority", ""),
                "lane": effective.get("lane", ""), "kind": kind, "manifest": relative,
                "test_file": test_file, "test_function": effective.get("test_function", ""),
                "command": effective.get("command", ""), "selection": selection,
                "required_evidence": effective.get("required_evidence", []),
                "covered": covered, "missing": missing, "details": effective,
                "manifest_sha256": digest, "test_sha256": _sha(entry_file) if entry_file else "",
            }
            if contract_id in contracts:
                previous = contracts[contract_id]
                previous["conflict"] = True
                previous.setdefault("duplicate_sources", []).append(entry)
                for key in ("covered", "missing"):
                    previous[key].extend(claim for claim in entry[key] if claim not in previous[key])
                warnings.append(f"Duplicate contract ID {contract_id}: {previous['manifest']} and {relative}; execution matching is ambiguous")
            else:
                contracts[contract_id] = entry
    if not manifest_files:
        warnings.append("No coverage manifests found under test/e2e/coverage")
    return {"modules": modules, "contracts": list(contracts.values()), "warnings": warnings,
            "manifest_files": manifest_files}
