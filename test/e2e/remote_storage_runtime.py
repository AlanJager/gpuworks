"""NFS browser registration through real agent mount, filesystem I/O and unmount.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: python3 test/e2e/remote_runtime_acceptance.py --config <private-json> --case storage
Environment/config: remote_runtime_acceptance.py module docstring.
Invoked by the explicit remote runtime runner; never runs against an implicit host.
"""
from __future__ import annotations

import hashlib
import json
import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check, modal_geometry


CONTRACT = "gpuworks/storage/remote-nfs-mount-roundtrip"

# This is host-side fixture/inspection code, not a replacement for agent mounting.
# mountinfo uses octal escaping; decode it before comparing exact paths/sources.
MOUNTS = r'''
import pathlib, re

def mounts():
    def decode(value):
        return re.sub(r"\\([0-7]{3})", lambda m: chr(int(m[1], 8)), value)
    entries = []
    for line in pathlib.Path('/proc/self/mountinfo').read_text().splitlines():
        left, right = line.split(' - ', 1)
        fields, fs = left.split(), right.split()
        entries.append({'path': decode(fields[4]), 'fstype': fs[0],
                        'source': decode(fs[1]), 'options': fields[5] + ',' + fs[2]})
    return entries
'''


def _mutation(r, action, method, path, body, status):
    with r.page.expect_response(
        lambda response: urlsplit(response.url).path == path and response.request.method == method,
        timeout=120000,
    ) as pending:
        action()
    response = pending.value
    check(response.status == status, f"{method} {path}: expected {status}, got {response.status}")
    check(not urlsplit(response.url).query, "unexpected mutation query")
    check(response.request.post_data_json == body if body is not None else response.request.post_data is None,
          f"{method} {path}: body differs from field contract")
    result = None if status == 204 else response.json()
    r.evidence("request", {"method": method, "path": path, "body": body, "status": status, "response": result})
    return result


# Mainline: gpuworks/storage/remote-nfs-mount-roundtrip; 自有 NFS 子目录和标记文件 → UI 指定节点试挂载并创建
# → 刷新及独立 GET 节点 ready → 内核 mountinfo + 双向文件 SHA256 → UI 删除 → agent 卸载且源文件保留 → scoped 清理。
def run_storage(r):
    t = json.loads((ROOT / "ui/src/i18n/locales/en/storage.json").read_text())
    common = json.loads((ROOT / "ui/src/i18n/locales/en/common.json").read_text())
    check(re.fullmatch(r"[A-Za-z0-9-]{1,48}", r.run_id) is not None, "unsafe run ID")
    name = f"e2e-{r.run_id.lower()}-nfs"
    node_uuid = r.config["node_uuid"]
    check(r.node["uuid"] == node_uuid, "host inspection node differs from selected node")
    node_name = r.node.get("name", "").strip() or r.node.get("hostname", "").strip() or node_uuid[:8]
    source_id = int(r.config["storage_source_id"])
    source = r.api("GET", f"/api/v1/storages/{source_id}?all_nodes=1")
    check(source["type"] in {"nfs", "nfs_cache"} and source["enabled"], "fixture source must be enabled NFS")
    source_nodes = [n for n in source["nodes"]["all"] if n["node_uuid"] == node_uuid]
    check(len(source_nodes) == 1 and source_nodes[0]["state"] == "ready" and source_nodes[0]["writable"],
          "fixture source must already be ready and writable on the selected node")
    source_root = source_nodes[0]["path"]
    source_dir = source_root.rstrip("/") + "/" + name
    export = source["export"].rstrip("/") + "/" + name
    mount_path = "/mnt/gpuworks-e2e/" + r.run_id
    source_marker = f"GPUWorks source {r.run_id}\n"
    mounted_marker = f"GPUWorks mounted {r.run_id}\n"
    source_hash = hashlib.sha256(source_marker.encode()).hexdigest()
    mounted_hash = hashlib.sha256(mounted_marker.encode()).hexdigest()
    fixture = {"source_path": source_dir, "mount_path": mount_path, "run_id": r.run_id}
    prepared = r.host(MOUNTS + f'''
import os
root = pathlib.Path({source_root!r})
owned = pathlib.Path({source_dir!r})
mount = pathlib.Path({mount_path!r})
assert os.geteuid() == 0, 'root agent host required for NFS fixture inspection'
assert root.is_absolute() and str(root) != '/' and root.is_dir() and root.resolve() == root, 'source root must be a real directory'
source_mount = [m for m in mounts() if m['path'] == str(root)]
assert len(source_mount) == 1 and source_mount[0]['fstype'] in ('nfs', 'nfs4'), 'source root is not mounted NFS'
assert source_mount[0]['source'].rstrip('/') == {source['export']!r}.rstrip('/'), 'source mount disagrees with storage export'
assert owned.parent == root and owned.name == {name!r} and not owned.exists() and not owned.is_symlink()
assert mount.parent.resolve() == mount.parent, 'test mount parent must not redirect through symlinks'
assert not mount.exists() and not mount.is_symlink(), 'test mount path already exists'
result = {{'source_mount': source_mount[0], 'source_path': str(owned), 'mount_path': str(mount)}}
''')
    r.own("storage_fixture", fixture)
    r.host(f'''
import pathlib
owned = pathlib.Path({source_dir!r})
assert owned.parent.resolve() == pathlib.Path({source_root!r})
owned.mkdir(exist_ok=False)
with (owned / 'from-source.txt').open('x') as stream:
    stream.write({source_marker!r})
result = {{'created': str(owned)}}
''')
    r.evidence("fixture", prepared)

    page = r.page
    page.goto(r.base + "/storage")
    expect(page.get_by_role("heading", name=t["page"]["title"], exact=True)).to_be_visible()
    page.get_by_role("button").filter(has=page.get_by_text(t["page"]["add"], exact=True)).click()
    dialog = page.get_by_role("dialog", name=t["form"]["addTitle"], exact=True)
    dialog.get_by_text(common["storageType"]["nfs"], exact=True).click()
    expect(dialog.get_by_role("radio", name=common["storageType"]["nfs"], exact=True)).to_be_checked()
    dialog.get_by_label(t["form"]["name"], exact=True).fill(name)
    dialog.get_by_label(t["form"]["export"], exact=True).fill(export)
    dialog.get_by_label(t["form"]["mountPoint"], exact=True).fill(mount_path)
    dialog.get_by_text(t["form"]["scope"]["nodes"], exact=True).click()
    expect(dialog.get_by_role("radio", name=t["form"]["scope"]["nodes"], exact=True)).to_be_checked()
    picker = dialog.get_by_role("combobox", name=t["form"]["scope"]["chooseNodes"], exact=True)
    picker.click()
    picker.fill(node_name)
    # AntD's virtual list exposes offscreen ARIA options; select through the
    # focused combobox's real keyboard behavior and verify the exact selection.
    option = page.get_by_role("option", name=node_name, exact=True)
    expect(option).to_have_count(1)
    picker.press("Enter")
    expect(option).to_have_attribute("aria-selected", "true")
    picker.press("Escape")
    expect(dialog.get_by_role("checkbox", name=t["form"]["scope"]["existing"].replace("{{node}}", node_name), exact=True)).not_to_be_checked()
    save = button(dialog, t["form"]["save"])
    expect(save).to_be_disabled()
    modal_geometry(dialog, save)
    probe_body = {"type": "nfs", "path": mount_path, "node_uuid": node_uuid, "export": export}
    probe = _mutation(r, button(dialog, t["form"]["probe"]).click, "POST", "/api/v1/storages/probe", probe_body, 200)
    check(probe["node_uuid"] == node_uuid and probe["ok"] and probe["readable"] and probe["writable"],
          "trial mount must really read and write on selected node")
    check(probe.get("version") in {"3", "4", "4.0", "4.1", "4.2"} and probe.get("options"),
          "trial mount must report kernel NFS version/options")
    check(probe["total_bytes"] > 0 and 0 <= probe["available_bytes"] <= probe["total_bytes"], "invalid probe capacity")
    expect(dialog.get_by_text(t["probe"]["ok"].replace("{{node}}", node_name), exact=True)).to_be_visible()
    expect(save).to_be_enabled()
    modal_geometry(dialog, save)
    # Trial mount is temporary: the real target may only appear after saving.
    before_save = r.host(MOUNTS + f"\nresult = [m for m in mounts() if m['source'] == {export!r} or m['path'] == {mount_path!r}]\n")
    check(before_save == [], "trial mount leaked a mount before storage creation")
    create_body = {"name": name, "type": "nfs", "all_nodes": False, "targets": [{"node_uuid": node_uuid}],
                   "path": mount_path, "export": export, "probe_node": node_uuid}
    created = _mutation(r, save.click, "POST", "/api/v1/storages", create_body, 201)
    storage_id = created["id"]
    r.own("storage", storage_id)
    check(storage_id != source_id, "created storage unexpectedly replaced source fixture")
    path = f"/api/v1/storages/{storage_id}"
    expect(page).to_have_url(r.base + f"/storage/{storage_id}")
    heading = page.get_by_role("heading").filter(has_text=name)
    expect(heading).to_be_visible()
    expect(heading).to_contain_text(common["storageType"]["nfs"])
    ready = r.poll(lambda: r.api("GET", path + "?all_nodes=1"),
                   lambda value: value["nodes"]["ready"] == 1 and len(value["nodes"].get("all", [])) == 1
                   and value["nodes"]["all"][0]["state"] == "ready"
                   and value["nodes"]["all"][0].get("total_bytes", 0) > 0, timeout=120)
    for key in ("name", "type", "all_nodes", "path", "export"):
        check(ready[key] == create_body[key], f"persisted {key} differs")
    check(ready["enabled"] and not ready["is_default"] and ready["nodes"]["total"] == 1, "unexpected storage scope/default")
    check(len(ready["targets"]) == 1 and ready["targets"][0]["node_uuid"] == node_uuid
          and not ready["targets"][0].get("existing", False), "storage did not retain exact managed-mount target")
    observed = ready["nodes"]["all"][0]
    check(observed["node_uuid"] == node_uuid and observed["path"] == mount_path and observed["writable"]
          and observed["total_bytes"] > 0, "agent mount observation differs")
    r.evidence("persistence", {"path": path, "storage": ready})
    r.evidence("execution", {"node_uuid": node_uuid, "observation": observed})
    page.reload()
    expect(page.get_by_role("heading").filter(has_text=name)).to_be_visible()
    page.get_by_role("tab", name=t["detail"]["tab"]["nodes"].replace("{{count}}", "1"), exact=True).click()
    row = page.get_by_role("row").filter(has=page.get_by_role("link", name=node_name, exact=True))
    expect(row).to_contain_text(common["storageNodeState"]["ready"])
    expect(row).to_contain_text(mount_path)
    expect(row).to_contain_text(t["nodes"]["yes"])
    r.evidence("ui", {"storage": name, "node": node_name, "state": "ready", "path": mount_path,
                      "trial_mount": "readable+writable", "reload": True, "dialog_viewport": True})

    roundtrip = r.host(MOUNTS + f'''
import hashlib, os
source = pathlib.Path({source_dir!r})
mounted = pathlib.Path({mount_path!r})
entries = [m for m in mounts() if m['path'] == str(mounted)]
assert len(entries) == 1 and entries[0]['fstype'] in ('nfs', 'nfs4')
assert entries[0]['source'].rstrip('/') == {export!r}.rstrip('/'), 'mount source differs'
def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()
assert sha(mounted / 'from-source.txt') == {source_hash!r}, 'source bytes did not traverse NFS mount'
with (mounted / 'from-mounted.txt').open('xb') as stream:
    stream.write({mounted_marker.encode()!r})
    stream.flush()
    os.fsync(stream.fileno())
assert sha(source / 'from-mounted.txt') == {mounted_hash!r}, 'mounted write did not reach source export'
result = {{'mount': entries[0], 'source_to_mount_sha256': sha(mounted / 'from-source.txt'),
          'mount_to_source_sha256': sha(source / 'from-mounted.txt')}}
''')
    check(roundtrip["source_to_mount_sha256"] == source_hash and roundtrip["mount_to_source_sha256"] == mounted_hash,
          "filesystem roundtrip hashes differ")
    r.evidence("runtime", roundtrip)

    page.get_by_role("button").filter(has=page.get_by_text(t["actions"]["more"], exact=True)).click()
    page.get_by_role("menuitem", name=t["actions"]["delete"], exact=True).click()
    confirm = page.get_by_role("dialog", name=t["actions"]["deleteConfirm"].replace("{{name}}", name), exact=True)
    expect(confirm.get_by_text(t["actions"]["deleteSharedHelp"], exact=True)).to_be_visible()
    modal_geometry(confirm, button(confirm, t["actions"]["delete"]))
    _mutation(r, button(confirm, t["actions"]["delete"]).click, "DELETE", path, None, 204)
    r.api("GET", path, expected=404)
    expect(page).to_have_url(r.base + "/storage")
    expect(page.get_by_role("link", name=name, exact=True)).to_have_count(0)
    r.poll(lambda: r.host(MOUNTS + f"\nresult = [m for m in mounts() if m['path'] == {mount_path!r}]\n"),
           lambda entries: entries == [], timeout=120)
    cleaned = r.host(MOUNTS + f'''
import hashlib, shutil
owned = pathlib.Path({source_dir!r})
mount = pathlib.Path({mount_path!r})
assert owned.parent == pathlib.Path({source_root!r}) and owned.name == {name!r}
assert owned.is_dir() and not owned.is_symlink() and owned.resolve() == owned
assert mount.parent == pathlib.Path('/mnt/gpuworks-e2e') and mount.name == {r.run_id!r}
assert mount.parent.resolve() == mount.parent
assert not mount.is_symlink()
assert not any(m['path'] == str(mount) or m['path'].startswith(str(mount) + '/')
               or m['path'] == str(owned) or m['path'].startswith(str(owned) + '/') for m in mounts())
assert hashlib.sha256((owned / 'from-source.txt').read_bytes()).hexdigest() == {source_hash!r}
assert hashlib.sha256((owned / 'from-mounted.txt').read_bytes()).hexdigest() == {mounted_hash!r}
shutil.rmtree(owned)
if mount.exists():
    mount.rmdir()
result = {{'agent_unmounted': True, 'source_files_preserved_before_fixture_cleanup': True,
          'owned_source_absent': not owned.exists(), 'mount_path_absent': not mount.exists()}}
''')
    check(cleaned["owned_source_absent"] and cleaned["mount_path_absent"], "owned filesystem fixture remained")
    source_after = r.api("GET", f"/api/v1/storages/{source_id}?all_nodes=1")
    for key in ("id", "name", "type", "enabled", "is_default", "all_nodes", "path", "export", "targets"):
        check(source_after[key] == source[key], f"existing source storage {key} changed")
    r.forget("storage", storage_id)
    r.forget("storage_fixture", fixture)
    r.evidence("cleanup", {"deleted_storage_id": storage_id, "http_status": 404, **cleaned,
                           "source_storage_id": source_id, "source_definition_unchanged": True})
