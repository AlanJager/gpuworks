#!/usr/bin/env python3
"""Real Registry acquisition, source selection and CPU image consumption.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-image-registry-pull / make test-e2e-image-registry-mirror
After make build: python3 test/e2e/image_registry.py --case registry-pull --locale en
Use --case registry-mirror and --locale zh for the other independent runs.
Requires Linux, Chrome, Python Playwright, sudo -n, unshare, Docker and static
busybox. Uses real server/SQLite/agent, an owned empty Docker daemon and local
Registry v2 fixtures; no existing node, GPU, public Registry or API mock.
Private reports/trace/logs and failed state: secrets/e2e/<runId>, retained 7 days.
"""
from pathlib import Path
from urllib.parse import urlsplit
import hashlib
import json

from playwright.sync_api import expect
from image_catalog import button, cleanup, observe, read_detail, strings
from image_registry_fixture import RegistryServer
from image_runtime import open_version, wait_node_state
from image_runtime_runner import run
from image_transfer_recovery import observe_navigation
from platform_token_read_revoke import check, collection


# Contracts: gpuworks/image-runtime/registry-pull, gpuworks/image-runtime/registry-mirror
# Mainline: 每个参数独立创建自有CPU agent、空Docker及Registry → UI显式选择canonical源或另一个Mirror并拉取 → 精确POST source、GET/刷新present与来源 → 只向所选Registry获取manifest/config/layer且digest逐层一致 → 以canonical引用运行真实无网络容器输出marker → 清理自有目录/Registry/节点/进程。
def test_registry_pull(page, server, fixture, artifacts, report, locale):
    text = strings(locale)
    mirror = report["contract"] == "gpuworks/image-runtime/registry-mirror"
    canonical = RegistryServer(fixture.archive.payload, "e2e-" + report["run_id"].lower())
    selected = canonical
    try:
        if mirror:
            selected = RegistryServer(fixture.archive.payload, canonical.repository)
        source = selected.origin
        expected_source = "mirror" if mirror else "direct"
        expected_label = text["nodeTable"]["sourceMirror"] if mirror else canonical.origin
        report["registry_fixture"] = {"canonical": canonical.image_ref, "selected": selected.image_ref,
            "image_id": selected.image_id, "manifest_digest": selected.manifest_digest}
        name = "e2e-" + report["run_id"]
        created = collection(page.request.post(server.base + "/api/v1/images/import", data={
            "series": [{"name": name, "engine": "callable", "source": "custom", "versions": [{
                "image_ref": canonical.image_ref, "arch": fixture.arch, "engine_version": "1",
                "source": "custom", "registry_sources": [selected.image_ref]}]}]}))
        check(len(created) == 1, "fixture must create exactly one series")
        series_id = created[0]["id"]
        before = read_detail(page, server.base, series_id)
        check(len(before["versions"]) == 1, "fixture must create exactly one version")
        version_id = before["versions"][0]["id"]
        initial = wait_node_state(page, server, series_id, version_id, fixture.node_uuid, "absent")
        check(initial["version"]["present_nodes"] == 0, "unpulled version counted as present")
        check(not fixture.docker(["image", "ls", "-q"]).stdout.strip(), "daemon has pre-existing images")
        check(not fixture.docker(["ps", "-aq"]).stdout.strip(), "daemon has pre-existing containers")
        errors, failures, mutations = observe(page)
        transports, cancellations, reload_page = observe_navigation(page)
        report["browser_observations"] = {"errors": errors, "http_failures": failures,
            "request_failures": transports, "expected_cancellations": cancellations, "mutations": mutations}
        reload_page()
        detail = open_version(page, text, name, canonical.image_ref)
        row = detail.get_by_role("row").filter(has_text=fixture.node_name)
        expect(row).to_have_count(1)
        expect(row.get_by_text(text["state"]["absent"], exact=True)).to_be_visible()
        selector = row.get_by_role("combobox", name=text["nodeTable"]["column"]["source"], exact=True)
        # Mirror runs first select the canonical source, then replace it with
        # the declared alternate. Submission must use the final UI choice.
        for host in ([canonical.origin, source] if mirror else [source]):
            selector.focus()
            selector.press("ArrowDown")
            # AntD exposes virtual options to assistive technology separately
            # from its visual popup. Use the named option's runtime identity
            # and real keyboard input, without clicking hidden DOM nodes.
            option = page.get_by_role("option", name=host, exact=True)
            option_id = option.get_attribute("id")
            check(option_id, "named Registry option has no accessible identity")
            for _ in range(3):
                if selector.get_attribute("aria-activedescendant") == option_id:
                    break
                selector.press("ArrowDown")
            expect(selector).to_have_attribute("aria-activedescendant", option_id)
            selector.press("Enter")
            expect(selector).to_have_attribute("aria-expanded", "false")
            expect(row.get_by_title(host, exact=True)).to_be_visible()
        path = f"/api/v1/images/{version_id}/nodes/{fixture.node_uuid}/sync"
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as started:
            button(row, text["nodeTable"]["pull"]).click()
        response = started.value
        check(response.status == 202 and urlsplit(response.url).query == "", "Registry pull was not accepted")
        check(response.request.post_data_json == {"source": source}, "selected Registry was not submitted exactly")
        transfer = response.json()
        check(transfer.get("transfer_id"), "Registry pull has no transfer identity")
        present = wait_node_state(page, server, series_id, version_id, fixture.node_uuid, "present")
        check(present["node"]["source"] == expected_source, "persisted acquisition source differs")
        check(present["node"]["image_digest_id"] == selected.image_id == fixture.image_id,
              "agent content ID differs from supplied config digest")
        check(present["version"]["present_nodes"] == 1, "acquired image counted incorrectly")
        expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible(timeout=30000)
        expect(row.get_by_text(expected_label, exact=True)).to_be_visible()
        reload_page()
        detail = open_version(page, text, name, canonical.image_ref)
        row = detail.get_by_role("row").filter(has_text=fixture.node_name)
        expect(row.get_by_text(text["state"]["present"], exact=True)).to_be_visible()
        expect(row.get_by_text(expected_label, exact=True)).to_be_visible()
        if mirror:
            expect(row.get_by_text(canonical.origin, exact=True)).to_have_count(0)
        persisted = wait_node_state(page, server, series_id, version_id, fixture.node_uuid, "present")
        check(persisted["node"]["source"] == expected_source and
              persisted["node"]["image_digest_id"] == selected.image_id, "reload lost source or content identity")
        inspected = json.loads(fixture.docker(["image", "inspect", canonical.image_ref]).stdout)
        check(len(inspected) == 1 and inspected[0]["Id"] == selected.image_id, "canonical tag resolves to wrong image")
        check(canonical.image_ref in inspected[0]["RepoTags"], "canonical tag missing after acquisition")
        repository_digest = selected.image_ref.rsplit(":", 1)[0] + "@" + selected.manifest_digest
        check(repository_digest in inspected[0]["RepoDigests"], "Docker manifest identity differs from chosen Registry")
        config = json.loads(selected.blobs[selected.image_id])
        check(inspected[0]["RootFS"]["Layers"] == config["rootfs"]["diff_ids"], "uncompressed layer identity differs")
        executed = fixture.docker(["run", "--rm", "--network", "none", "--pull=never", canonical.image_ref])
        check(executed.returncode == 0 and executed.stdout == fixture.marker + "\n",
              "acquired canonical image cannot execute the expected CPU payload")
        check(not fixture.docker(["ps", "-aq"]).stdout.strip(), "consumer container leaked")
        audit = selected.snapshot()
        manifest_gets = [r for r in audit if r["method"] == "GET" and r["status"] == 200
            and r["path"].startswith(f"/v2/{selected.repository}/manifests/")]
        check(any(r["digest"] == selected.manifest_digest and r["bytes_sent"] == len(selected.manifest_bytes) for r in manifest_gets),
              "Docker never fetched the selected manifest")
        for descriptor in [selected.manifest["config"], *selected.manifest["layers"]]:
            blob = selected.blobs[descriptor["digest"]]
            check(len(blob) == descriptor["size"] and "sha256:" + hashlib.sha256(blob).hexdigest() == descriptor["digest"],
                  "Registry descriptor size/digest differs from content")
            check(any(r["method"] == "GET" and r["status"] == 200 and
                      r["path"] == f"/v2/{selected.repository}/blobs/{descriptor['digest']}" and
                      r["bytes_sent"] == descriptor["size"] for r in audit), "Docker did not fetch the complete declared blob")
        check(all(r["status"] in (200, 206) for r in audit), "unexpected Registry request failure")
        if mirror:
            check(canonical.snapshot() == [], "explicit mirror silently accessed the canonical Registry")
            check(canonical.connection_count == 0, "explicit mirror connected to the unselected Registry")
        check(fixture.archive.snapshot() == [], "Registry selection incorrectly used the archive download path")
        check(not errors and not failures and not transports, "unexpected browser or request error")
        check([m for m in mutations if "/nodes/" in m["path"]] == [{"method": "POST", "path": path}],
              "duplicate or unexpected node mutation")
        report["evidence"].extend([
            {"layer": "ui", "selected_origin": source, "completed_source_label": expected_label,
             "present_after_reload": True, "canonical_to_mirror_selection": mirror},
            {"layer": "request", "method": "POST", "path": path, "body": {"source": source},
             "status": response.status, "response": transfer},
            {"layer": "persistence", "detail": persisted["detail"]},
            {"layer": "runtime", "docker_image_id": inspected[0]["Id"], "repo_digests": inspected[0]["RepoDigests"],
             "rootfs_diff_ids": inspected[0]["RootFS"]["Layers"], "manifest": selected.manifest,
             "registry_requests": audit, "unselected_registry_requests": canonical.snapshot() if mirror else [],
             "archive_requests": [], "container_exit": executed.returncode, "stdout": executed.stdout}])
        cleanup(page, server.base, series_id, report)
    finally:
        report["registry_requests"] = {"canonical": canonical.snapshot(), "selected": selected.snapshot()}
        if selected is not canonical:
            selected.stop()
        canonical.stop()
        report["registry_connections"] = {"canonical": canonical.connection_count, "selected": selected.connection_count}
        report["registry_servers_stopped"] = True


if __name__ == "__main__":
    raise SystemExit(run({"registry-pull": test_registry_pull, "registry-mirror": test_registry_pull}, Path(__file__),
        runtime_description="Real Registry v2 manifest/config/layer pull and CPU execution; no public Registry/peer/GPU/inference claim",
        extra_sources=["test/e2e/image_registry_fixture.py", "test/e2e/image_runtime.py", "test/e2e/image_transfer_recovery.py",
            "ui/src/pages/images/ImageNodeTable.tsx", "ui/src/pages/images/ImagePageHelpers.tsx",
            "ui/src/pages/ImagesPage.tsx", "ui/src/i18n/locales/en/images.json", "ui/src/i18n/locales/zh/images.json",
            "internal/server/hub/image_pull.go", "internal/agent/app/control.go", "internal/agent/runtime/docker/docker.go",
            "internal/server/imagecatalog/pullref/pullref.go"]))
