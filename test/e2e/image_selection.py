#!/usr/bin/env python3
"""Image source selection contracts through real Chrome, server and SQLite.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Build/run: make test-e2e-image-community-prefill / test-e2e-image-archive-prefill
After building: python3 test/e2e/image_selection.py --case community-prefill --locale en
Cases: community-prefill, archive-prefill. Locale: en or zh.
Requires Chrome, Python Playwright and openssl. No GPU, agent or Docker.
The private TLS fixture supplies OCI tag-list JSON and minimal docker-save
archive metadata; it does not represent a runnable image or image pull.
Server trust is scoped to this process environment, never installed system-wide.
Artifacts and failed database: secrets/e2e/<runId>; remove within 7 days.
"""
from __future__ import annotations

import os
import re
from pathlib import Path
import tempfile
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect
from image_catalog import button, cleanup, read_detail, select, strings
from image_catalog_runner import run
from image_selection_fixture import ImageSelectionFixture
from platform_token_read_revoke import check, collection

fixture = None


def observations(page):
    errors, failures, imports, console_errors = [], [], [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("console", lambda message: console_errors.append({"text": message.text, "url": message.location.get("url", "")}) if message.type == "error" else None)
    page.on("response", lambda response: failures.append({"path": urlsplit(response.url).path, "status": response.status}) if response.status >= 400 else None)
    page.on("request", lambda request: imports.append(request.post_data_json) if request.method == "POST" and urlsplit(request.url).path == "/api/v1/images/import" else None)
    return errors, failures, imports, console_errors


# Contract: gpuworks/image-catalog/community-prefill
# Mainline: 独立server配置可审计TLS仓库A/B → UI选择A nightly、手改引擎版本、过滤再换B release → 精确POST与独立GET/刷新证明B预填替换A、人工框架版本保留；清理目录和fixture，无镜像拉取声明。
def test_community_prefill(page, server, artifacts, report, locale):
    fixture.configure(report["run_id"])
    text, source = strings(locale), "community"
    form = text["import"]
    name = "e2e-" + report["run_id"]
    repos = [{"repo": repo, "registry": "oci", "upstream_api": fixture.origin + "/tags/" + suffix,
              "engine": engine, "framework": "cuda"}
        for repo, suffix, engine in [(fixture.repo_a, "a", "vllm"), (fixture.repo_b, "b", "sglang")]]
    response = page.request.put(server.base + "/api/v1/settings/image_community_catalog", data={"repos": repos})
    check(response.status == 200, "owned community catalog setup failed")
    errors, failures, imports, console_errors = observations(page)
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    button(page, text["page"]["addCommunity"]).click()
    dialog = page.get_by_role("dialog", name=form["title"][source], exact=True)
    expect(dialog).to_be_in_viewport(ratio=1)
    engine_options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    labels = {row["name"]: row["label"] for row in engine_options}
    with page.expect_response(lambda r: "/images/community/browse" in r.url) as first:
        button(dialog, fixture.repo_a).click()
    check(first.value.status == 200, "first community browse failed")
    a = first.value.json()
    check(parse_qs(urlsplit(first.value.url).query) == {"repo": [fixture.repo_a]}, "wrong repository query")
    check(a["engine"] == "vllm" and a["framework"] == "cuda", "catalog metadata lost")
    check({tag["tag"] for tag in a["tags"]} == {"nightly", "v1.2.3"}, "upstream tags differ")
    button(dialog, "nightly").click()
    expect(dialog.get_by_label(form["imageRef"], exact=True)).to_have_value(fixture.repo_a + ":nightly")
    expect(dialog.get_by_label(form["engineVersion"], exact=True)).to_have_value("")
    expect(dialog.get_by_role("switch", name=form["rollingTag"], exact=True)).to_be_checked()
    expect(dialog.get_by_text(labels["vllm"], exact=True)).to_be_visible()
    expect(dialog.get_by_text("CUDA", exact=True)).to_be_visible()
    # Editing is intentional; a tag search does not choose a new image or reset the draft.
    dialog.get_by_label(form["engineVersion"], exact=True).fill("operator-version")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    search = dialog.get_by_role("textbox", name=form["community"]["search"], exact=True)
    search.fill("missing-tag")
    expect(dialog.get_by_text(form["community"]["noMatch"], exact=True)).to_be_visible()
    expect(dialog.get_by_label(form["engineVersion"], exact=True)).to_have_value("operator-version")
    search.fill("")
    expect(button(dialog, "nightly")).to_be_visible()
    with page.expect_response(lambda r: "/images/community/browse" in r.url) as second:
        button(dialog, fixture.repo_b).click()
    check(second.value.status == 200, "second community browse failed")
    b = second.value.json()
    check(parse_qs(urlsplit(second.value.url).query) == {"repo": [fixture.repo_b]}, "wrong replacement repository")
    chosen = next(tag for tag in b["tags"] if tag["tag"] == "v4.5.6")
    check(b["engine"] == "sglang" and b["framework"] == "cuda" and chosen["engine_version"] == "4.5.6", "second authoritative metadata differs")
    button(dialog, chosen["tag"]).click()
    expect(dialog.get_by_label(form["imageRef"], exact=True)).to_have_value(fixture.refs[1])
    expect(dialog.get_by_label(form["engineVersion"], exact=True)).to_have_value(chosen["engine_version"])
    expect(dialog.get_by_text(labels[b["engine"]], exact=True)).to_be_visible()
    expect(dialog.get_by_role("switch", name=form["rollingTag"], exact=True)).not_to_be_checked()
    expect(dialog.get_by_label(form["frameworkVersion"], exact=True)).to_have_value("12.8")
    dialog.get_by_label(form["seriesName"], exact=True).fill(name)
    expected = {"series": [{"name": name, "engine": b["engine"], "framework": b["framework"], "source": source,
        "versions": [{"engine_version": chosen["engine_version"], "framework_version": "12.8", "arch": "multi",
        "image_ref": fixture.refs[1], "source": source, "rolling_tag": False, "registry_sources": []}]}]}
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/import" and r.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.request.post_data_json == expected, "selection submitted stale or incorrect metadata")
    check(urlsplit(imported.value.url).query == "", "unexpected import query")
    result = collection(imported.value)
    check(len(result) == 1, "must create exactly one series")
    detail = read_detail(page, server.base, result[0]["id"])
    check(detail["engine"] == "sglang" and detail["framework"] == "cuda" and detail["source"] == source, "persisted series differs")
    check(len(detail["versions"]) == 1, "unexpected version count")
    saved = detail["versions"][0]
    for key, value in expected["series"][0]["versions"][0].items():
        check(saved.get(key, [] if key == "registry_sources" else False if key == "rolling_tag" else None) == value, f"persisted {key} differs")
    check(not any(v["image_ref"].startswith(fixture.repo_a + ":") for v in collection(page.request.get(server.base + "/api/v1/images/versions"))), "unsubmitted A became persisted")
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    button(page, name).click()
    details = page.get_by_role("dialog", name=text["detail"]["title"].replace("{{name}}", name), exact=True)
    expect(details).to_be_in_viewport(ratio=1)
    expect(details.get_by_role("link", name=fixture.refs[1], exact=True)).to_be_visible()
    check(read_detail(page, server.base, result[0]["id"]) == detail, "refresh changed metadata")
    check(not errors and not failures and not console_errors and imports == [expected], "unexpected browser error, failed HTTP or duplicate import")
    artifacts.write_json("browser-observations.json", {"errors": errors, "console_errors": console_errors, "failures": failures, "imports": imports})
    check([r["path"] for r in fixture.records] == ["/tags/a", "/tags/b"], "unexpected external registry access")
    report["evidence"].extend([{"layer": "ui", "selected": [fixture.repo_a + ":nightly", fixture.refs[1]], "manual_value_survived_search": True},
        {"layer": "request", "status": imported.value.status, "body": expected, "browse": [a, b], "external_requests": fixture.records},
        {"layer": "persistence", "detail": detail, "reload": True}])
    cleanup(page, server.base, result[0]["id"], report)
    check(page.request.put(server.base + "/api/v1/settings/image_community_catalog", data={"repos": []}).status == 200, "catalog cleanup failed")
    check(fixture.close(), "external fixture failed to stop")
    report["external_fixture_stopped"] = True


# Contract: gpuworks/image-catalog/archive-prefill
# Mainline: 可审计A/B archive → UI解析A、人工覆盖引用、B真实503保留草稿、恢复解析B → POST仅含B引用/地址，GET与刷新证明持久化；不把archive预览当成拉取或容器可用。
def test_archive_prefill(page, server, artifacts, report, locale):
    fixture.configure(report["run_id"])
    errors, failures, imports, console_errors = observations(page)
    text, source = strings(locale), "custom"
    form = text["import"]
    name = "e2e-" + report["run_id"]
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    button(page, text["page"]["addCustom"]).click()
    dialog = page.get_by_role("dialog", name=form["title"][source], exact=True)
    expect(dialog).to_be_in_viewport(ratio=1)
    address = dialog.get_by_role("textbox", name=form["url"]["label"], exact=True)
    reference = dialog.get_by_label(form["imageRef"], exact=True)
    url_a, url_b = fixture.origin + "/a.tar", fixture.origin + "/b.tar?download=1"
    address.fill(url_a)
    with page.expect_response(lambda r: "/images/preview-archive-url" in r.url) as first:
        button(dialog, form["url"]["resolve"]).click()
    check(first.value.status == 200 and first.value.json()["repo_tags"] == [fixture.refs[0]], "archive A preview differs")
    check(first.value.request.post_data_json == {"url": url_a}, "archive A request differs")
    expect(reference).to_have_value(fixture.refs[0])
    reference.fill(f"registry.example/{name}/manual:v7")
    address.fill(url_b)
    expect(reference).to_have_value(f"registry.example/{name}/manual:v7")
    fixture.failed = True
    with page.expect_response(lambda r: "/images/preview-archive-url" in r.url) as failed:
        button(dialog, form["url"]["resolve"]).click()
    check(failed.value.request.method == "POST" and urlsplit(failed.value.url).path == "/api/v1/images/preview-archive-url" and not urlsplit(failed.value.url).query and failed.value.request.post_data_json == {"url": url_b}, "failed preview request differs")
    check(failed.value.status == 502, "upstream 503 must become real server 502")
    check(failed.value.json()["code"] == "IMAGE_ARCHIVE_URL_UNREACHABLE", "wrong archive failure code")
    expect(dialog.get_by_role("alert").filter(has_text="HTTP 502 · IMAGE_ARCHIVE_URL_UNREACHABLE")).to_be_visible()
    expect(reference).to_have_value(f"registry.example/{name}/manual:v7")
    check(collection(page.request.get(server.base + "/api/v1/images?source=custom")) == [], "preview unexpectedly persisted an image")
    fixture.failed = False
    with page.expect_response(lambda r: "/images/preview-archive-url" in r.url) as second:
        button(dialog, form["url"]["resolve"]).click()
    check(second.value.request.method == "POST" and urlsplit(second.value.url).path == "/api/v1/images/preview-archive-url" and not urlsplit(second.value.url).query and second.value.request.post_data_json == {"url": url_b}, "recovery preview request differs")
    check(second.value.status == 200, "archive recovery failed")
    expect(dialog.get_by_role("alert").filter(has_text="HTTP 502 · IMAGE_ARCHIVE_URL_UNREACHABLE")).to_have_count(0)
    preview = second.value.json()
    check(preview["repo_tags"] == [fixture.refs[1]] and preview["url"] == url_b and preview["previewable"], "archive B preview differs")
    expect(reference).to_have_value(fixture.refs[1])
    expect(dialog.get_by_text(url_b, exact=True)).to_be_visible()
    expect(dialog.get_by_text(url_a, exact=True)).to_have_count(0)
    dialog.get_by_label(form["seriesName"], exact=True).fill(name)
    options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    select(dialog.get_by_label(form["engine"], exact=True), next(v["label"] for v in options if v["name"] == "vllm"))
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    expected = {"series": [{"name": name, "engine": "vllm", "framework": "cuda", "source": source,
        "versions": [{"engine_version": "", "framework_version": "12.8", "arch": "multi", "image_ref": fixture.refs[1], "source": source,
        "rolling_tag": False, "registry_sources": [url_b]}]}]}
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/import" and r.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.request.post_data_json == expected, "archive import submitted stale A source or wrong fields")
    result = collection(imported.value)
    check(len(result) == 1, "archive import must create one series")
    detail = read_detail(page, server.base, result[0]["id"])
    check(detail["engine"] == "vllm" and detail["framework"] == "cuda" and len(detail["versions"]) == 1, "archive series differs")
    saved = detail["versions"][0]
    for key, value in expected["series"][0]["versions"][0].items():
        check(saved.get(key, False if key == "rolling_tag" else None) == ("4.5.6" if key == "engine_version" else value), f"archive persisted {key} differs")
    check(saved["engine_version"] == "4.5.6", "server must infer version from the actual B reference")
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    button(page, name).click()
    details = page.get_by_role("dialog", name=text["detail"]["title"].replace("{{name}}", name), exact=True)
    expect(details).to_be_in_viewport(ratio=1)
    expect(details.get_by_role("link", name=fixture.refs[1], exact=True)).to_be_visible()
    check(read_detail(page, server.base, result[0]["id"]) == detail, "archive reload differs")
    check(all(error["url"] == failed.value.url and re.fullmatch(r"Failed to load resource: the server responded with a status of 502 \(Bad Gateway\)", error["text"]) for error in console_errors), "unexpected console error")
    check(not errors and failures == [{"path": "/api/v1/images/preview-archive-url", "status": 502}] and imports == [expected], "unexpected browser error, HTTP failure or duplicate import")
    artifacts.write_json("browser-observations.json", {"errors": errors, "console_errors": console_errors, "failures": failures, "imports": imports})
    check(any(r["method"] == "HEAD" and r["status"] == 503 for r in fixture.records), "no real upstream failure")
    for path in ["/a.tar", "/b.tar?download=1"]:
        check(any(r["path"] == path and r["method"] == "GET" and r["range"] and r["status"] == 206 for r in fixture.records), "no real archive range read")
    report["evidence"].extend([{"layer": "ui", "manual_draft_retained_on_failure": True, "replacement": fixture.refs[1]},
        {"layer": "request", "body": expected, "status": imported.value.status, "preview": preview, "failed_status": 502, "external_requests": fixture.records},
        {"layer": "persistence", "detail": detail, "reload": True}])
    cleanup(page, server.base, result[0]["id"], report)
    check(fixture.close(), "external fixture failed to stop")
    report["external_fixture_stopped"] = True


if __name__ == "__main__":
    import uuid
    with tempfile.TemporaryDirectory(prefix="gpuworks-image-selection-") as directory:
        fixture = ImageSelectionFixture(Path(directory), uuid.uuid4().hex[:12])
        previous = os.environ.get("SSL_CERT_FILE")
        os.environ["SSL_CERT_FILE"] = str(fixture.cert)
        try:
            result = run({"community-prefill": test_community_prefill, "archive-prefill": test_archive_prefill},
                entry_path=__file__, product_sources=("test/e2e/image_selection_fixture.py", "test/e2e/image_catalog.py",
                "ui/src/pages/images/ImageCommunityBrowser.tsx", "ui/src/pages/images/ImageArchiveURLImport.tsx",
                "internal/server/api/images_catalog_import.go", "internal/server/api/images_community.go",
                "internal/server/api/images_archive_url.go"))
        finally:
            fixture.close()
            if previous is None:
                os.environ.pop("SSL_CERT_FILE", None)
            else:
                os.environ["SSL_CERT_FILE"] = previous
    raise SystemExit(result)
