"""Apply bounded fixes for the exact upstream images pinned in runner-images/matrix.yaml."""
import importlib.metadata
import json
import os
from pathlib import Path
import subprocess
import sys


def main():
    if sys.argv[1] == "sglang":
        from prepare_sglang import prepare
        prepare()
        return
    if sys.argv[1] != "vllm":
        return
    # This base exposes Ubuntu's Python 3.10 GI distribution to Python 3.12.
    # Its Cairo dependency is missing and its native extension has the wrong ABI.
    # Remove the complete unused distro packages through apt, including their
    # reverse dependencies, rather than hide metadata from the component audit.
    expected = {"networkd-dispatcher", "python3-gi",
                "python3-software-properties", "software-properties-common"}
    if sys.version_info[:2] != (3, 12) or importlib.metadata.version("PyGObject") != "3.42.1":
        raise RuntimeError("upstream Python/GI versions differ from the pinned repair")
    env = dict(os.environ, LC_ALL="C", DEBIAN_FRONTEND="noninteractive")
    command = ["apt-get", "--no-download", "--no-auto-remove", "remove", "python3-gi"]
    plan = subprocess.run(command[:1] + ["--simulate"] + command[1:],
                          env=env, check=True, capture_output=True, text=True).stdout
    removed = {line.split()[1] for line in plan.splitlines() if line.startswith("Remv ")}
    if removed != expected or any(line.startswith(("Inst ", "Conf ")) for line in plan.splitlines()):
        raise RuntimeError("unexpected apt transaction; refusing to change the base: " + plan)
    versions = subprocess.run(
        ["dpkg-query", "-W", "-f=${Package}=${Version}\n", *sorted(expected)],
        check=True, capture_output=True, text=True).stdout.splitlines()
    subprocess.run(command[:1] + ["--assume-yes"] + command[1:], env=env, check=True)
    for package in expected:
        status = subprocess.run(["dpkg-query", "-W", "-f=${Status}", package],
                                capture_output=True, text=True)
        if status.stdout.strip() == "install ok installed":
            raise RuntimeError("apt left a package installed: " + package)
    try:
        importlib.metadata.version("PyGObject")
    except importlib.metadata.PackageNotFoundError:
        pass
    else:
        raise RuntimeError("incompatible PyGObject distribution remains visible")
    record = {"reason": "Python 3.10 distro GI package exposed to Python 3.12",
              "removed_distro_packages": versions}
    Path("/opt/gpuworks/base-adjustments.json").write_text(json.dumps(record, indent=2) + "\n")
    print(json.dumps(record), flush=True)


if __name__ == "__main__":
    main()
