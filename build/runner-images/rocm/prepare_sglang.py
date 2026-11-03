"""Repair the pinned SGLang ROCm dependency graph without replacing Torch."""
import hashlib
import importlib.metadata as metadata
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import urllib.request


TVM_WHEEL = (
    "https://files.pythonhosted.org/packages/f3/e3/"
    "436b573a23ef171c14e90181ff379819ef8481d8236fda9afb29b3b15516/"
    "apache_tvm_ffi-0.1.10-cp310-cp310-manylinux_2_24_x86_64.manylinux_2_28_x86_64.whl",
    "5dda14f065520c3bdec6889fecfa7c1b06a4f6fb23e7b2475d9a0477eb8588d8",
)
ADDON_SOURCE = (
    "https://files.pythonhosted.org/packages/37/de/"
    "921b6491efce5c389a5ef9bbed3d2d6660005840dae488124173180859ab/"
    "torch_c_dlpack_ext-0.1.5.tar.gz",
    "d06f0357d575d22a168cc77acb9020fc4bae30968ceb6718a055dcbe92bacabe",
)


def fetch(root, artifact):
    url, expected = artifact
    path = root / url.rsplit("/", 1)[1]
    with urllib.request.urlopen(url, timeout=120) as response:
        data = response.read()
    if hashlib.sha256(data).hexdigest() != expected:
        raise RuntimeError("dependency artifact SHA256 mismatch: " + path.name)
    path.write_bytes(data)
    return path


def prepare():
    expected = {
        "sglang": "0.5.21",
        "xgrammar": "0.2.7",
        "tilelang": "0.1.7.post3+cuda.gita55a8230",
        "apache-tvm-ffi": "0.1.8.post3.dev42+g37d0485b2",
    }
    before = {name: metadata.version(name) for name in expected}
    if sys.version_info[:2] != (3, 10) or before != expected:
        raise RuntimeError("SGLang base differs from the pinned dependency repair: " + str(before))
    retained = {name: metadata.version(name) for name in
                ("torch", "triton", "sglang", "sglang-kernel", "tilelang", "xgrammar")}
    pip = [sys.executable, "-m", "pip"]
    with tempfile.TemporaryDirectory(prefix="gpuworks-rocm-") as temporary:
        root = Path(temporary)
        wheel = fetch(root, TVM_WHEEL)
        source = fetch(root, ADDON_SOURCE)
        subprocess.run(pip + ["install", "--no-index", "--no-deps", "--no-cache-dir", str(wheel)], check=True)
        package = root / "torch_c_dlpack_ext-0.1.5"
        with tarfile.open(source) as archive:
            for member in archive.getmembers():
                target = (root / member.name).resolve()
                if not target.is_relative_to(package) or not (member.isfile() or member.isdir()):
                    raise RuntimeError("unexpected addon source archive member: " + member.name)
            archive.extractall(root)
        output = package / "torch_c_dlpack_ext"
        # Disable automatic fallback only in build subprocesses. Runtime keeps
        # TVM's ordinary DLPack behavior and gets real CPU and HIP libraries.
        build_env = dict(os.environ, TVM_FFI_DISABLE_TORCH_C_DLPACK="1")
        helper = [sys.executable, "-m", "tvm_ffi.utils._build_optional_torch_c_dlpack",
                  "--output-dir", str(output)]
        cpu = "libtorch_c_dlpack_addon_torch29-cpu.so"
        # The 0.1.5 loader uses the name "cuda" for torch.cuda.is_available(),
        # including HIP. Compile that filename with ROCm, never CUDA libraries.
        hip = "libtorch_c_dlpack_addon_torch29-cuda.so"
        subprocess.run(helper + ["--build-dir", str(root / "build-cpu"), "--libname", cpu],
                       env=build_env, check=True)
        subprocess.run(helper + ["--build-dir", str(root / "build-rocm"),
                                 "--build-with-rocm", "--libname", hip], env=build_env, check=True)
        libraries = {}
        for name in (cpu, hip):
            path = output / name
            dynamic = subprocess.run(["readelf", "-d", str(path)], check=True,
                                     capture_output=True, text=True).stdout
            needed = [line.split("[", 1)[1].split("]", 1)[0]
                      for line in dynamic.splitlines() if "(NEEDED)" in line]
            if any("cuda" in item.lower() for item in needed):
                raise RuntimeError("CUDA dependency in ROCm addon: " + str(needed))
            libraries[name] = {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                               "needed": needed}
        wheels = root / "wheels"
        # The sdist otherwise calls a prebuilt ctypes library pure Python.
        # Mark our wheel native so its tags restrict Python ABI and platform.
        (package / "setup.py").write_text(
            "from setuptools import Distribution, setup\n"
            "class NativeDistribution(Distribution):\n"
            "    def has_ext_modules(self):\n"
            "        return True\n"
            "setup(distclass=NativeDistribution)\n")
        subprocess.run([sys.executable, "setup.py", "bdist_wheel", "--plat-name=linux_x86_64",
                        "--python-tag=cp310", "--dist-dir", str(wheels)],
                       cwd=package, env=build_env, check=True)
        built = wheels / "torch_c_dlpack_ext-0.1.5-cp310-cp310-linux_x86_64.whl"
        if not built.is_file():
            raise RuntimeError("expected the native cp310 Linux amd64 addon wheel")
        subprocess.run(pip + ["install", "--no-index", "--no-deps", "--no-cache-dir", str(built)], check=True)
    if any(metadata.version(name) != version for name, version in retained.items()):
        raise RuntimeError("dependency repair changed a retained engine/GPU package")
    record = {
        "reason": "satisfy XGrammar TVM-FFI minimum and build the missing TileLang DLPack dependency for ROCm",
        "before": before,
        "after": {"apache-tvm-ffi": metadata.version("apache-tvm-ffi"),
                  "torch-c-dlpack-ext": metadata.version("torch-c-dlpack-ext")},
        "retained_packages": retained,
        "artifacts": [{"url": url, "sha256": digest} for url, digest in (TVM_WHEEL, ADDON_SOURCE)],
        "native_libraries": libraries,
    }
    Path("/opt/gpuworks/base-adjustments.json").write_text(json.dumps(record, indent=2) + "\n")
    print(json.dumps(record), flush=True)
