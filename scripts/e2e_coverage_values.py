"""Frozen, reviewed value sets; declarations never replace execution evidence.

Unlike source-specific automatic enum extraction, these sets are manually frozen
with source hashes. Any source drift or conflicting denominator makes them
unknown. Python assertion anchors make declarations auditable, not a proof that
all branches of a page or helper were executed.

Anchors are scored under a deliberately narrow static model. Assertions inside
except-handler or try-else bodies, after unbounded polling loops, or inside
loops over an enumerated literal-empty iterable — empty []/()/{} literals,
empty str/bytes, zero-argument set/list/tuple/dict() calls, or
literal-argument range() with a provably non-positive count — never score;
such declarations stay visibly unknown instead of being silently dropped.
Any other iterable keeps may-execute.
"""
from __future__ import annotations

import ast
import hashlib
import re

from e2e_coverage_inventory import _safe_file

# ast.TryStar is the except* statement, which exists only from Python 3.11.
# An interpreter without it cannot be handed source that uses except* either,
# so falling back to ast.Try loses no case and keeps the scanner importable on
# 3.10.
TRY_STATEMENTS = (ast.Try, getattr(ast, "TryStar", ast.Try))

ASSERTIONS = {"check", "checked", "same", "assert_equal", "assert_eq"}
IDENTIFIER = re.compile(r"[A-Za-z][A-Za-z0-9_.:-]{0,119}\Z")
SHA256 = re.compile(r"[0-9a-f]{64}\Z")


def _file(root, name):
    try:
        return _safe_file(root, name)
    except (ValueError, OSError, RuntimeError):
        return None


def _snapshot(root, sources):
    if not isinstance(sources, list) or not sources:
        return None
    result = []
    for item in sources:
        if not isinstance(item, dict):
            return None
        file, expected = item.get("file"), item.get("sha256")
        if not isinstance(file, str) or not isinstance(expected, str) or not SHA256.fullmatch(expected):
            return None
        path = _file(root, file)
        try:
            matches = path and path.is_file() and hashlib.sha256(path.read_bytes()).hexdigest() == expected
        except OSError:
            matches = False
        if not matches:
            return None
        result.append((file, expected))
    return tuple(sorted(result)) if len(set(result)) == len(result) else None


def _message(node):
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return [node.value]
    if isinstance(node, ast.JoinedStr):
        return [v.value for v in node.values if isinstance(v, ast.Constant) and isinstance(v.value, str)]
    return []


PURE = {"len", "bool", "str", "int", "float", "list", "tuple", "set", "dict",
        "min", "max", "sum", "all", "any", "sorted", "range", "abs", "round"}


def _constant(node):
    """Constant unless a Name load proves runtime dependence. Anything without a
    non-builtin Name load — literals, dict-unpacking, method calls on literals,
    walrus, starred, unknown node shapes — is constant by construction.

    Accepted boundary: a Name load is runtime data without propagation, so a
    local or module-level name bound to a plain literal still scores
    (`x = 1; checked(x == 1, ...)`, `EXPECTED == EXPECTED`). Distinguishing
    those needs real dataflow analysis, and real anchors legitimately compare
    runtime values against local literal dicts (e.g. a full-wire expected
    body). The file:line anchor emitted per scored value is the human-audit
    backstop for this boundary."""
    return all(child.id in PURE for child in ast.walk(node)
               if isinstance(child, ast.Name) and isinstance(child.ctx, ast.Load))


def _truth(node):
    """Truth value of a statically literal condition; None when unknown."""
    if isinstance(node, ast.Constant):
        return bool(node.value)
    if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
        value = _truth(node.operand)
        return None if value is None else not value
    return None


def _int(node):
    """Literal integer value, unwrapping unary minus/plus; None when not literal."""
    if isinstance(node, ast.Constant) and isinstance(node.value, int):
        return node.value
    if isinstance(node, ast.UnaryOp) and isinstance(node.op, (ast.USub, ast.UAdd)):
        value = _int(node.operand)
        if value is not None:
            return -value if isinstance(node.op, ast.USub) else value
    return None


def _empty_range(node):
    """Provably empty range() with literal integer arguments; anything else —
    non-literal bounds, keyword arguments, a zero step — keeps may-execute."""
    if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "range"
            and not node.keywords and 1 <= len(node.args) <= 3):
        return False
    values = [_int(a) for a in node.args]
    if any(v is None for v in values):
        return False
    start, stop, step = (0, values[0], 1) if len(values) == 1 else (*values, 1) if len(values) == 2 else values
    return step > 0 and stop <= start or step < 0 and stop >= start


def _empty(node):
    """Provably empty literal iterable; anything unproven keeps may-execute."""
    if isinstance(node, ast.Constant):
        return isinstance(node.value, (str, bytes)) and not node.value
    if isinstance(node, (ast.List, ast.Tuple, ast.Set)):
        return not node.elts
    if isinstance(node, ast.Dict):
        return not node.keys
    if _empty_range(node):
        return True
    return (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
            and node.func.id in {"set", "list", "tuple", "dict"} and not node.args and not node.keywords)


def _live_nodes(function):
    """Fail-closed liveness under a deliberately narrow model: plain sequential
    statements, constant-folded if, loops, with, and try bodies contribute;
    return/raise/break/continue and unprovable while-const termination end a
    block, except/match and unrecognized statements stop it. Anything doubtful
    is simply never scored."""

    def descend(node):
        yield node
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda)):
                continue
            yield from descend(child)

    def visit(statements):
        live = True
        for statement in statements:
            if not live:
                return
            if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                continue
            if isinstance(statement, ast.If):
                yield statement
                yield from descend(statement.test)
                truth = _truth(statement.test)
                if truth is not False:
                    yield from visit(statement.body)
                if truth is not True:
                    yield from visit(statement.orelse)
                continue
            if isinstance(statement, (ast.For, ast.AsyncFor)):
                yield statement
                yield from descend(statement.target)
                yield from descend(statement.iter)
                if not _empty(statement.iter):
                    yield from visit(statement.body)
                yield from visit(statement.orelse)
                continue
            if isinstance(statement, ast.While):
                yield statement
                yield from descend(statement.test)
                truth = _truth(statement.test)
                if truth is False:
                    yield from visit(statement.orelse)
                    continue
                if truth is not None:
                    yield from visit(statement.body)
                    live = False
                    continue
                yield from visit(statement.body)
                yield from visit(statement.orelse)
                continue
            if isinstance(statement, (ast.With, ast.AsyncWith)):
                yield statement
                for item in statement.items:
                    yield from descend(item)
                yield from visit(statement.body)
                continue
            if isinstance(statement, TRY_STATEMENTS):
                yield statement
                yield from visit(statement.body)
                yield from visit(statement.finalbody)
                continue
            if isinstance(statement, (ast.Expr, ast.Assign, ast.AnnAssign, ast.AugAssign, ast.Assert,
                                      ast.Return, ast.Raise, ast.Pass, ast.Break, ast.Continue,
                                      ast.Import, ast.ImportFrom, ast.Delete, ast.Global, ast.Nonlocal)):
                yield statement
                for child in ast.iter_child_nodes(statement):
                    yield from descend(child)
                if isinstance(statement, (ast.Return, ast.Raise, ast.Break, ast.Continue)):
                    live = False
                continue
            return

    yield from visit(function.body)


def _scope_bindings(function):
    """Fail-closed scope model: a name bound anywhere in the function scope by
    anything — targets, imports, nested defs, except-as, match captures, del,
    global/nonlocal declarations — never provably resolves to a module helper."""
    bound = {arg.arg for arg in (*function.args.posonlyargs, *function.args.args, *function.args.kwonlyargs)}
    bound.update(target.arg for target in (function.args.vararg, function.args.kwarg) if target)
    pending = list(function.body)
    while pending:
        node = pending.pop()
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            bound.add(node.name)
            continue
        if isinstance(node, ast.Lambda):
            continue
        if isinstance(node, ast.Name) and isinstance(node.ctx, (ast.Store, ast.Del)):
            bound.add(node.id)
        elif isinstance(node, (ast.Import, ast.ImportFrom)):
            bound.update(alias.asname or alias.name.split(".", 1)[0] for alias in node.names)
        elif isinstance(node, ast.ExceptHandler) and node.name:
            bound.add(node.name)
        elif isinstance(node, (ast.MatchAs, ast.MatchStar)) and node.name:
            bound.add(node.name)
        elif isinstance(node, ast.MatchMapping) and node.rest:
            bound.add(node.rest)
        elif isinstance(node, (ast.Global, ast.Nonlocal)):
            bound.update(node.names)
        pending.extend(ast.iter_child_nodes(node))
    return bound


def _module_rebound(tree):
    """A helper name bound by anything but its single def — module-level store,
    del, import or capture, or a global rebinding inside any function — no
    longer provably refers to that function."""
    rebound = set()
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        for child in ast.walk(node):
            if isinstance(child, ast.Name) and isinstance(child.ctx, (ast.Store, ast.Del)):
                rebound.add(child.id)
            elif isinstance(child, (ast.Import, ast.ImportFrom)):
                rebound.update(alias.asname or alias.name.split(".", 1)[0] for alias in child.names)
            elif isinstance(child, ast.ExceptHandler) and child.name:
                rebound.add(child.name)
            elif isinstance(child, (ast.MatchAs, ast.MatchStar)) and child.name:
                rebound.add(child.name)
            elif isinstance(child, ast.MatchMapping) and child.rest:
                rebound.add(child.rest)
    for function in (n for n in ast.walk(tree) if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))):
        declared = {name for child in ast.walk(function) if isinstance(child, ast.Global) for name in child.names}
        stored = {child.id for child in ast.walk(function)
                  if isinstance(child, ast.Name) and isinstance(child.ctx, (ast.Store, ast.Del))}
        rebound.update(declared & stored)
    return rebound


def _reachable(tree, functions, entry):
    """Only statically resolved live same-file calls; shadowed or dead paths stay unknown."""
    rebound = _module_rebound(tree)
    pending, found = [entry], set()
    while pending:
        name = pending.pop()
        if name in found or len(functions.get(name, [])) != 1 or name in rebound:
            continue
        found.add(name)
        function = functions[name][0]
        shadowed = _scope_bindings(function)
        pending.extend(node.func.id for node in _live_nodes(function)
                       if isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                       and node.func.id in functions and node.func.id not in shadowed)
    return found


def _anchor(root, binding, contract):
    if not isinstance(binding, dict):
        return None
    file, name, label = (binding.get(key) for key in ("file", "function", "assertion"))
    if not all(isinstance(v, str) and v.strip() for v in (file, name, label)):
        return None
    path = _file(root, file)
    entry_file, entry_name = contract.get("test_file"), contract.get("test_function")
    if (not path or path.suffix != ".py" or not isinstance(entry_file, str)
            or not isinstance(entry_name, str) or path != _file(root, entry_file)):
        return None
    try:
        tree = ast.parse(path.read_text())
    except (OSError, UnicodeError, SyntaxError):
        return None
    functions = {}
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            functions.setdefault(node.name, []).append(node)
    if name not in _reachable(tree, functions, entry_name):
        return None
    shadowed = _scope_bindings(functions[name][0])
    for node in _live_nodes(functions[name][0]):
        if isinstance(node, ast.Assert):
            tests, messages = [node.test], _message(node.msg)
        elif (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
              and node.func.id in ASSERTIONS and node.func.id not in shadowed and len(node.args) >= 2):
            equality = node.func.id in {"same", "assert_equal", "assert_eq"}
            if equality and len(node.args) < 3:
                continue
            tests = node.args[:-1] if equality else [node.args[0]]
            messages = _message(node.args[-1])
        else:
            continue
        if all(_constant(test) for test in tests) or label not in messages:
            continue
        return f"{file}:{node.lineno} {name}: {label}"
    return None


def value_rows(root, data, metric):
    """Merge only consistent frozen sets; bind each value to its owning contracts."""
    groups, poisoned = {}, {}
    for contract in data.get("contracts", []):
        if contract.get("kind") != "business":
            continue
        pending, seen = [contract], set()
        conflicted = bool(contract.get("conflict") or contract.get("duplicate_sources"))
        while pending:
            source = pending.pop()
            if not isinstance(source, dict) or id(source) in seen:
                continue
            seen.add(id(source))
            duplicates = source.get("duplicate_sources", [])
            if isinstance(duplicates, list):
                pending.extend(duplicates)
            details = source.get("details", {})
            declarations = details.get("component_value_coverage", []) if isinstance(details, dict) else []
            if not isinstance(declarations, list):
                continue
            for declaration in declarations:
                if not isinstance(declaration, dict):
                    continue
                identifier, scope = declaration.get("id"), declaration.get("scope")
                if not isinstance(identifier, str) or not IDENTIFIER.fullmatch(identifier):
                    continue
                good_scope = isinstance(scope, str) and scope in {"product", "fixture"}
                tainted = conflicted or not good_scope
                if tainted:
                    poisoned.setdefault(identifier, set()).add(str(source.get("manifest", contract["manifest"])))
                group_scope = scope if good_scope else "unknown"
                groups.setdefault((contract["module"], identifier, group_scope), []).append((source, declaration, tainted))
    rows = []
    for (_, identifier, scope), entries in sorted(groups.items()):
        first = entries[0][1]
        universe = first.get("universe")
        valid = (isinstance(universe, list) and bool(universe)
                 and all(isinstance(v, str) and v.strip() for v in universe)
                 and len(set(universe)) == len(universe))
        known_values = set(universe) if isinstance(universe, list) and all(isinstance(v, str) for v in universe) else None
        snapshot = _snapshot(root, first.get("sources"))
        valid = (valid and snapshot is not None and identifier not in poisoned
                 and isinstance(first.get("basis"), str) and bool(first["basis"].strip()))
        owners, selected, anchors, problems = {}, set(), [], []
        contracts = {c["id"]: c for c, _, _ in entries}
        allowed_contracts = {c["id"]: c for c, _, tainted in entries if not tainted}
        for contract, declaration, tainted in entries:
            if (declaration.get("universe") != universe or _snapshot(root, declaration.get("sources")) != snapshot
                    or declaration.get("name") != first.get("name") or declaration.get("basis") != first.get("basis")):
                valid = False
            covered = declaration.get("covered")
            if not isinstance(covered, dict) or known_values is None or set(covered) - known_values:
                valid = False
                continue
            for value, bindings in covered.items():
                found = [_anchor(root, b, contract) for b in bindings] if isinstance(bindings, list) else []
                if not found or any(a is None for a in found):
                    problems.append(f"{value} 的断言绑定缺失或不可核对；未计为已覆盖")
                    continue
                selected.add(value)
                anchors.extend(found)
                if not tainted:
                    owners.setdefault(value, set()).add(contract["id"])
        basis = ("人工冻结的" + {"product": "产品选项/状态", "fixture": "本 fixture 可达集合"}.get(scope, "未知适用集合")
                 + "；须核对源码快照与函数内断言锚点，非自动发现或路径穷举。" + str(first.get("basis", "")))
        row = metric(list(contracts.values()), "values-" + identifier, first.get("name") or identifier,
                     universe if valid else None, selected if valid else [], basis, sorted(set(anchors)),
                     contract_map={v: sorted(owners.get(v, [])) for v in universe} if valid else {})
        row["id"] = "values#" + entries[0][0]["module"] + "/" + identifier + "/" + scope
        row["contract_ids"] = list(allowed_contracts)
        row["unknown"].extend(sorted(set(problems)))
        if identifier in poisoned:
            row["unknown"].append("冲突或无效声明来源：" + ", ".join(sorted(poisoned[identifier])))
        if not valid:
            row["unknown"].append("冻结分母无效、源码快照已变化或声明冲突；保留未知，不缩减分母")
        rows.append(row)
    return rows
