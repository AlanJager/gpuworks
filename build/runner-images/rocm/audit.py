"""Offline component check; successful imports do not certify GPU inference."""
import contextlib
import importlib
import importlib.metadata
import json
import os
from pathlib import Path
import platform
import subprocess
import sys


def sglang_cpu_check(torch):
    # Some upstream imports print diagnostics; keep the audit's stdout JSON.
    with contextlib.redirect_stdout(sys.stderr):
        import torch_c_dlpack_ext
        import tvm_ffi
        import xgrammar
        import tilelang  # Injects the bundled TVM Python path.
        import tvm
        import sgl_kernel
        compiler = xgrammar.GrammarCompiler(xgrammar.TokenizerInfo(["a"]))
        grammar = compiler.compile_grammar(xgrammar.Grammar.from_ebnf('root ::= "a"'))
        matcher = xgrammar.GrammarMatcher(grammar)
        assert matcher.accept_string("a") and matcher.is_completed()
        variable = tvm.tir.Var("x", "int32")
        assert str(variable)
        source = torch.tensor([1, 2, 3], dtype=torch.int64)
        restored = torch.from_dlpack(tvm_ffi.from_dlpack(source))
        assert torch.equal(source, restored)
        return {name: importlib.metadata.version(name) for name in
                ("apache-tvm-ffi", "torch-c-dlpack-ext", "tilelang", "xgrammar", "sglang-kernel")}


def main():
    engine = os.environ["GPUWORKS_EXPECT_ENGINE"]
    expected = os.environ["GPUWORKS_EXPECT_ENGINE_VERSION"]
    report = {
        "schema": "gpuworks.amd-image-audit.v1",
        "engine": engine,
        "expected_version": expected,
        "architecture": platform.machine(),
        "python": platform.python_version(),
        "hardware_validation": "pending",
        "checks": {},
    }
    checks = report["checks"]
    adjustments = Path("/opt/gpuworks/base-adjustments.json")
    if adjustments.exists():
        report["base_adjustments"] = json.loads(adjustments.read_text())
    try:
        version = importlib.metadata.version(engine)
        report["engine_version"] = version
        checks["engine_version"] = version.split("+", 1)[0] == expected
        importlib.import_module(engine)
        checks["engine_import"] = True
    except Exception as exc:
        checks["engine_import"] = False
        report["engine_error"] = repr(exc)
    try:
        import torch
        report["torch"] = {"version": torch.__version__, "hip": torch.version.hip,
                           "cuda": torch.version.cuda}
        checks["rocm_torch"] = bool(torch.version.hip) and torch.version.cuda is None
    except Exception as exc:
        checks["rocm_torch"] = False
        report["torch_error"] = repr(exc)
    if engine == "sglang":
        try:
            report["sglang_dependencies"] = sglang_cpu_check(torch)
            checks["sglang_native_cpu"] = True
        except Exception as exc:
            checks["sglang_native_cpu"] = False
            report["sglang_native_cpu_error"] = repr(exc)
    markers = {}
    for path in sorted(Path("/opt").glob("rocm*/.info/version*")):
        if path.is_file():
            markers[str(path)] = path.read_text(errors="replace").strip()
    report["rocm_markers"] = markers
    checks["rocm_version_marker"] = bool(markers)
    try:
        result = subprocess.run([sys.executable, "-m", "pip", "check"],
                                capture_output=True, text=True, timeout=120)
        report["pip_check"] = {"exit": result.returncode,
                               "output": result.stdout + result.stderr}
        checks["dependencies"] = result.returncode == 0
    except (OSError, subprocess.TimeoutExpired) as exc:
        checks["dependencies"] = False
        report["pip_check"] = {"error": repr(exc)}
    report["status"] = "pass" if all(checks.values()) else "fail"
    print(json.dumps(report, indent=2))
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    sys.exit(main())
