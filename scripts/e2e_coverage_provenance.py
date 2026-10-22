"""Check declared source fingerprints without importing or executing test code."""
import ast
import hashlib
from pathlib import Path


def source_file(root, relative):
    if not isinstance(relative, str):
        return None
    path = Path(relative)
    if path.is_absolute() or any(p in {"..", ".git", "secrets"} for p in path.parts):
        return None
    resolved = (root / path).resolve()
    return resolved if resolved.is_relative_to(root.resolve()) and resolved.is_file() else None


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _literal_path(node, current, contract):
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.Name) and node.id == "__file__":
        return current
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Div):
        if isinstance(node.left, ast.Name) and node.left.id == "ROOT":
            return _literal_path(node.right, current, contract)
        left, right = _literal_path(node.left, current, contract), _literal_path(node.right, current, contract)
        return str(Path(left) / right) if left and right else None
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "Path" and len(node.args) == 1:
        return _literal_path(node.args[0], current, contract)
    if isinstance(node, ast.Subscript) and isinstance(node.value, ast.Name) and node.value.id == "manifest":
        if isinstance(node.slice, ast.Constant) and node.slice.value == "test_file":
            return contract.get("test_file")
    return None


def scalar_bindings(root, contract):
    """Recognize literal digest calls in the declared entry/driver and main delegation."""
    paths = [contract.get("test_file"), contract.get("details", {}).get("driver_file")]
    bindings, seen = {}, set()
    while paths:
        relative = paths.pop()
        path = source_file(root, relative)
        if not path or path in seen:
            continue
        seen.add(path)
        try:
            tree = ast.parse(path.read_text())
        except (OSError, UnicodeError, SyntaxError):
            continue
        imports = {}
        for node in tree.body:
            if isinstance(node, ast.Import):
                for alias in node.names:
                    imports[alias.asname or alias.name] = alias.name
        for node in ast.walk(tree):
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) and node.func.attr == "main":
                if isinstance(node.func.value, ast.Name) and node.func.value.id in imports:
                    candidate = path.parent / (imports[node.func.value.id].replace(".", "/") + ".py")
                    if candidate.is_relative_to(root):
                        paths.append(str(candidate.relative_to(root)))
            pairs = []
            if isinstance(node, ast.Dict):
                pairs = list(zip(node.keys, node.values))
            elif isinstance(node, ast.Assign):
                pairs = [(target.slice, node.value) for target in node.targets if isinstance(target, ast.Subscript)]
            for key, value in pairs:
                if not isinstance(key, ast.Constant) or not isinstance(key.value, str) or not key.value.endswith("_sha256"):
                    continue
                if not isinstance(value, ast.Call) or not isinstance(value.func, ast.Name) or value.func.id != "digest" or len(value.args) != 1:
                    continue
                resolved = _literal_path(value.args[0], str(path.relative_to(root)), contract)
                if resolved and source_file(root, resolved):
                    bindings.setdefault(key.value, set()).add(resolved)
    return bindings


def _model_fixture_paths(root, value):
    """The replica protocol records paths relative to the shared MODEL_DIR."""
    declaration = source_file(root, "test/e2e/deployment_lifecycle.py")
    if not declaration:
        return []
    try:
        tree = ast.parse(declaration.read_text())
    except (OSError, UnicodeError, SyntaxError):
        return []
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == "MODEL_DIR" for target in node.targets):
            directory = _literal_path(node.value, str(declaration.relative_to(root)), {})
            if directory:
                return [(str(Path(directory) / relative), expected) for relative, expected in value.items()]
    return []


def fingerprints(report, contract, root):
    reasons, supplied = [], {}
    # These hashes describe runtime bytes, not repository source files.
    runtime_fields = {"binary_sha256", "agent_binary_sha256", "server_binary_sha256",
                      "archive_sha256", "override_dockerfile_sha256", "original_package_sha256", "original_yaml_sha256"}
    scalar = scalar_bindings(root, contract)
    scalar.setdefault("test_sha256", set()).add(contract.get("test_file"))
    for field in ("coverage_manifest_sha256", "manifest_sha256"):
        scalar[field] = {contract["manifest"]}
    explicit = report.get("case_fixture_file")
    if source_file(root, explicit) and explicit.startswith("test/e2e/"):
        scalar["case_fixture_sha256"] = {explicit}
    for key, value in report.items():
        if key in runtime_fields or not key.endswith("_sha256") and key != "source_hashes":
            continue
        if key == "fixture_model_file_sha256" and isinstance(value, dict):
            entries = _model_fixture_paths(root, value)
            if not entries:
                reasons.append("模型夹具文件摘要无法绑定")
        elif isinstance(value, dict):
            entries = value.items()
        elif key in scalar and len(scalar[key]) == 1:
            entries = [(next(iter(scalar[key])), value)]
        else:
            reasons.append("存在无法安全绑定路径的源码摘要字段")
            continue
        for relative, expected in entries:
            path = source_file(root, relative)
            if not path or not isinstance(expected, str) or len(expected) != 64 or any(c not in "0123456789abcdef" for c in expected):
                reasons.append("源码摘要路径或格式无效")
            elif digest(path) != expected:
                reasons.append("报告绑定的源码已变化")
            supplied[relative] = expected
    for relative in (contract["manifest"], contract.get("test_file")):
        if not relative or relative not in supplied:
            reasons.append("清单或用例摘要缺失")
    return list(dict.fromkeys(reasons))
