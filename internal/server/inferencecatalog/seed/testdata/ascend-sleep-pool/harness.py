#!/usr/bin/env python3
"""Report what the level-1 sleep kv_cache pool actually covers, or apply the patch.

The crash this guards against is a device-memory fault that only reproduces on
a real 910C, so the hermetic assertion here is structural: which statements end
up inside the discardable ``kv_cache`` pool. That is exactly the defect --
persistent metadata allocated inside the pool is freed by a level-1 sleep while
keeping its virtual address -- so the scope is the property worth pinning.
Whether the device actually survives sleep is a hardware test, not one of these.

Modes:

  scope <vllm_ascend-root>   print one `KEY value` line per observed fact
  patch <script> <root>      import the catalog patch script and run it

Stdlib only, so the suite stays offline.
"""
import argparse
import ast
import importlib.util
from pathlib import Path
import sys

WORKER = "worker/worker.py"
RUNNER = "worker/model_runner_v1.py"

# The statements whose pool membership decides whether sleep is safe. The KV
# backing allocation is the only one that may sit inside the discardable pool.
ALLOCATION = "_allocate_kv_cache_tensors"
PERSISTENT = ("_reshape_kv_cache_tensors", "initialize_attn_backend",
              "may_reinitialize_input_batch")


def function(tree, name):
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == name:
            return node
    raise SystemExit(f"harness: {name} not found")


def mentions(node, needle):
    return any(isinstance(child, ast.Name) and child.id == needle
               or isinstance(child, ast.Attribute) and child.attr == needle
               for child in ast.walk(node))


def pool_withs(node):
    """`with` statements in node whose context manager is a sleep-mode pool."""
    found = []
    for child in ast.walk(node):
        if not isinstance(child, ast.With):
            continue
        for item in child.items:
            expression = item.context_expr
            if (mentions(expression, "use_memory_pool")
                    or mentions(expression, "kv_cache_allocation_context")
                    or mentions(expression, "pool")
                    or mentions(expression, "context")):
                found.append(child)
                break
    return found


def calls_inside(statement, needle):
    return any(isinstance(child, ast.Call) and mentions(child.func, needle)
               for child in ast.walk(statement))


def scope(root):
    root = Path(root)
    facts = {}

    worker = ast.parse((root / WORKER).read_text())
    from_config = function(worker, "initialize_from_config")
    # Does the worker hand the pool to the runner, or wrap the whole call?
    forwards = any(
        isinstance(node, ast.Call)
        and mentions(node.func, "initialize_kv_cache")
        and any(keyword.arg == "kv_cache_allocation_context" for keyword in node.keywords)
        for node in ast.walk(from_config))
    facts["worker.forwards-pool"] = "yes" if forwards else "no"
    wrapped = [w for w in pool_withs(from_config) if calls_inside(w, "initialize_kv_cache")]
    # A fallback `with` for runners this patch does not cover is allowed only
    # when the forwarding path exists too; without forwarding it is the defect.
    facts["worker.wraps-initialize-kv-cache"] = "yes" if wrapped and not forwards else "no"

    runner = ast.parse((root / RUNNER).read_text())
    tensors = function(runner, "initialize_kv_cache_tensors")
    facts["runner.accepts-pool"] = (
        "yes" if any(argument.arg == "kv_cache_allocation_context"
                     for argument in tensors.args.args) else "no")
    pools = pool_withs(tensors)
    facts["runner.allocation-in-pool"] = (
        "yes" if any(calls_inside(w, ALLOCATION) for w in pools) else "no")
    leaked = sorted({name for w in pools for name in PERSISTENT if calls_inside(w, name)})
    facts["runner.persistent-in-pool"] = ",".join(leaked) if leaked else "none"

    init = function(runner, "initialize_kv_cache")
    facts["runner.init-forwards-pool"] = (
        "yes" if any(
            isinstance(node, ast.Call)
            and mentions(node.func, "initialize_kv_cache_tensors")
            and any(keyword.arg == "kv_cache_allocation_context" for keyword in node.keywords)
            for node in ast.walk(init)) else "no")
    return facts


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["scope", "patch"])
    parser.add_argument("first")
    parser.add_argument("second", nargs="?")
    arguments = parser.parse_args()
    if arguments.mode == "scope":
        for key, value in sorted(scope(arguments.first).items()):
            print(f"{key} {value}")
        return
    spec = importlib.util.spec_from_file_location("catalog_patch", arguments.first)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.patch_root(arguments.second)


if __name__ == "__main__":
    sys.exit(main())
