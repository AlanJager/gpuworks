"""Real Docker tools layer and files for the container override browser case.

The engine binary is copied unchanged from the audited fake-engine image.
Alpine supplies an exec shell only; all image inputs must already exist locally.
"""
import json
import hashlib
from pathlib import Path
import re
import subprocess
import sqlite3

import deployment_wizard_fixture as wizard
import deployment_container_fixture as containers
from platform_token_read_revoke import check

SHM_BYTES = 268435456


def command(args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=120)
    check(result.returncode == 0, f"{args[:2]} failed: {result.stderr}")
    return result.stdout


def prepare(server, token, run_id, report, holder, agent_bin, remember):
    image = "gpuworks/e2e-overrides:" + run_id.lower()
    # Register before build: a lost build response can still leave the tag.
    report["owned_override_image"] = image
    build = server.work / "override-image"
    build.mkdir()
    sources = []
    for index, source in enumerate(("alpine:3.20", wizard.IMAGE)):
        identity = json.loads(command(["docker", "image", "inspect", source]))[0]["Id"]
        pinned = image + "-source-" + str(index)
        report.setdefault("owned_override_source_tags", []).append(pinned)
        command(["docker", "tag", identity, pinned])
        sources.append({"source": source, "image_id": identity, "owned_tag": pinned})
    report["override_image_sources"] = sources
    dockerfile = (
        "FROM " + sources[0]["owned_tag"] + "\n"
        "COPY --from=" + sources[1]["owned_tag"] + " /usr/local/bin/ /usr/local/bin/\n"
        'ENTRYPOINT ["/usr/local/bin/fake-engine"]\n')
    (build / "Dockerfile").write_text(dockerfile)
    report["override_dockerfile_sha256"] = hashlib.sha256(dockerfile.encode()).hexdigest()
    command(["docker", "build", "--pull=false", "--network=none", "-t", image, str(build)])
    report["override_image_id"] = command(
        ["docker", "image", "inspect", "--format", "{{.Id}}", image]).strip()
    fixture = wizard.prepare(server, token, run_id, report, holder, agent_bin, remember)
    fixture["image"] = image
    fixture["override_value"] = "browser override " + run_id + " 中文 = value"
    fixture["read_bytes"] = ("read-only fixture " + run_id + " 中文\n").encode()
    fixture["write_bytes"] = ("written inside container " + run_id + "\n").encode()
    for name in ("ro", "rw"):
        directory = server.storage_root / ("override-" + name)
        directory.mkdir()
        fixture[name + "_dir"] = str(directory)
    Path(fixture["ro_dir"], "known.txt").write_bytes(fixture["read_bytes"])
    return fixture


def expected_overrides(fixture):
    return {"env": {"E2E_WIZARD": fixture["override_value"]},
            "env_unset": ["HF_HUB_OFFLINE"], "shm_bytes": SHM_BYTES,
            "extra_mounts": [
                {"host_path": fixture["ro_dir"], "container_path": "/e2e-ro",
                 "purpose": "data", "read_only": True},
                {"host_path": fixture["rw_dir"], "container_path": "/e2e-rw",
                 "purpose": "data", "read_only": False}]}


def stored_overrides(server, dep_id):
    # The public DTO intentionally exposes resolved instance launch facts,
    # rather than the user's raw override layer. Observe that field read-only;
    # mode=ro still sees committed WAL content (immutable would not).
    database = server.work / "server-data/gpuworks.db"
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True) as connection:
        rows = connection.execute("SELECT container_overrides FROM deployments WHERE id = ?", (dep_id,)).fetchall()
    check(len(rows) == 1, f"expected one run-owned persisted deployment {dep_id}")
    return json.loads(rows[0][0])


def verify_runtime(container, fixture, report):
    inspect = json.loads(command(["docker", "inspect", container]))[0]
    check(inspect["Config"]["Image"] == fixture["image"], "runtime image reference changed")
    check(inspect["Image"] == report["override_image_id"], "runtime image identity changed")
    check(inspect["HostConfig"]["ShmSize"] == SHM_BYTES, "Docker shm limit differs")
    mounts = {item["Destination"]: item for item in inspect["Mounts"]}
    for name, writable in (("ro", False), ("rw", True)):
        mount = mounts["/e2e-" + name]
        check(mount["Type"] == "bind" and mount["Source"] == fixture[name + "_dir"]
              and mount["RW"] is writable, f"actual {name} mount differs: {mount}")
    script = '''
set -eu
test "$(id -u)" = 0
printf '%s' "$E2E_WIZARD"
test "${HF_HUB_OFFLINE+x}" != x
test "${TRANSFORMERS_OFFLINE}" = 1
'''
    actual = command(["docker", "exec", container, "/bin/sh", "-c", script])
    check(actual == fixture["override_value"], f"container exec environment differs: {actual!r}")
    read = subprocess.check_output(["docker", "exec", container, "cat", "/e2e-ro/known.txt"], timeout=10)
    check(read == fixture["read_bytes"], "read-only mount exposed different bytes")
    refused = subprocess.run(["docker", "exec", container, "/bin/sh", "-c",
        "printf forbidden > /e2e-ro/refused.txt"], capture_output=True, text=True, timeout=10)
    check(refused.returncode != 0 and "Read-only file system" in refused.stderr,
          f"read-only mount did not reject write: {refused.returncode} {refused.stderr}")
    check(not Path(fixture["ro_dir"], "refused.txt").exists(), "read-only write reached host")
    command(["docker", "exec", "-i", container, "/bin/sh", "-c",
             "printf '%s\\n' \"$1\" > /e2e-rw/written.txt", "sh",
             fixture["write_bytes"].decode().rstrip("\n")])
    check(Path(fixture["rw_dir"], "written.txt").read_bytes() == fixture["write_bytes"],
          "writable mount did not write exact host bytes")
    observed = command(["docker", "exec", container, "stat", "-f", "-c", "%S %b", "/dev/shm"]).split()
    check(len(observed) == 2 and int(observed[0]) * int(observed[1]) == SHM_BYTES,
          f"container tmpfs size differs: {observed}")
    return {"image": fixture["image"], "image_id": inspect["Image"], "env": actual,
            "unset": "HF_HUB_OFFLINE", "retained": "TRANSFORMERS_OFFLINE=1",
            "ro_bytes_hex": read.hex(), "ro_write": "EROFS",
            "rw_bytes_hex": fixture["write_bytes"].hex(), "shm_bytes": SHM_BYTES}


def inventory(report):
    image = report.get("owned_override_image")
    if not image:
        return
    candidates = command(["docker", "ps", "-aq", "--filter", "ancestor=" + image]).split()
    for ident in candidates:
        observed = json.loads(command(["docker", "inspect", ident]))[0]
        # Different tags can point to the same cached image ID. Only the
        # exact run-owned launch reference confers container ownership.
        if observed["Config"]["Image"] == image and ident not in report.setdefault("containers", []):
            report["containers"].append(ident)


def cleanup(server, token, report, holder):
    okay = False
    residuals = []
    def observe_inventory(after=False):
        try:
            inventory(report)
            if after:
                for ident in report.get("containers", []):
                    check(containers.docker_running(ident) == "absent", "owned container remains: " + ident)
        except Exception as error:
            residuals.append({"kind": "container-inventory", "id": report.get("owned_override_image"),
                "state": str(error), "reclaim": "inspect/remove only containers whose Config.Image equals this exact run-owned tag"})
    observe_inventory()
    try:
        okay = wizard.cleanup(server, token, report, holder)
    finally:
        observe_inventory(after=True)
        report.setdefault("remaining_resources", []).extend(residuals)
        okay = okay and not residuals
        images = ([report["owned_override_image"]] if report.get("owned_override_image") else [])
        images += list(reversed(report.get("owned_override_source_tags", [])))
        for image in images:
            try:
                result = subprocess.run(["docker", "image", "rm", image], capture_output=True, text=True, timeout=20)
                observed = subprocess.run(["docker", "image", "inspect", image], capture_output=True, text=True, timeout=10)
                missing = re.fullmatch(r"(?:Error response from daemon: )?No such image: " + re.escape(image),
                                       observed.stderr.strip(), re.IGNORECASE)
                check(observed.returncode != 0 and missing is not None,
                      f"image cleanup not observed: {result.stderr}; {observed.stderr}")
                report.setdefault("override_images_removed", []).append(image)
                if image == report.get("owned_override_image"):
                    report["owned_override_image"] = None
                else:
                    report["owned_override_source_tags"].remove(image)
            except Exception as error:
                okay = False
                report.setdefault("remaining_resources", []).append({"kind": "image", "id": image,
                    "state": str(error), "reclaim": "docker image rm " + image})
    return okay
