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
from dialog_checks import assert_drawer_shell
from image_catalog import button, cleanup, read_detail, select, strings
from image_catalog_runner import run
from image_selection_fixture import ImageSelectionFixture
from platform_token_read_revoke import check, collection

fixture = None


def _form_item(control):
    return control.locator(
        "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")


def _select_root(control):
    return control.locator(
        "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-select ')][1]")


def _reject_field(dialog, form, imports, control, message, quiet):
    before = len(imports)
    button(dialog, form["addImage"]).click()
    errors = _form_item(control).locator(".ant-form-item-explain-error")
    expect(errors).to_have_count(1)
    expect(errors).to_have_text(message)
    for other in quiet:
        expect(_form_item(other).locator(".ant-form-item-explain-error")).to_have_count(0)
    check(len(imports) == before, "required-field rejection sent an image import")


def _clear_select(control):
    root = _select_root(control)
    root.hover()
    clear = root.locator(".ant-select-clear")
    expect(clear).to_be_visible()
    clear.click()
    expect(root.locator(".ant-select-selection-item")).to_have_count(0)


def exercise_required_fields(page, dialog, form, imports, report, base, step):
    """Each required field rejects an empty value, and each text field rejects whitespace, with no import."""
    options = collection(page.request.get(base + "/api/v1/images/engines"))
    engine_label = next(row["label"] for row in options if row["name"] == "vllm")
    reference = dialog.get_by_label(form["imageRef"], exact=True)
    engine = dialog.get_by_label(form["engine"], exact=True)
    framework = dialog.get_by_label(form["framework"], exact=True)
    framework_version = dialog.get_by_label(form["frameworkVersion"], exact=True)
    reference.fill("registry.example/team/required:v1")
    select(engine, engine_label)
    select(framework, "CUDA")
    framework_version.fill("12.8")

    reference.fill("")
    report["last_step"] = step + "-required-reference-empty"
    _reject_field(dialog, form, imports, reference, form["imageRefRequired"], [engine, framework, framework_version])
    reference.fill("   ")
    report["last_step"] = step + "-required-reference-blank"
    _reject_field(dialog, form, imports, reference, form["tagRequired"], [engine, framework, framework_version])
    reference.fill("registry.example/team/required:v1")

    framework_version.fill("")
    report["last_step"] = step + "-required-framework-version-empty"
    _reject_field(dialog, form, imports, framework_version, form["frameworkVersionRequired"], [reference, engine, framework])
    framework_version.fill("   ")
    report["last_step"] = step + "-required-framework-version-blank"
    _reject_field(dialog, form, imports, framework_version, form["frameworkVersionRequired"], [reference, engine, framework])
    framework_version.fill("12.8")

    check(engine.get_attribute("readonly") is not None, "the engine combobox accepts typed text")
    check(framework.get_attribute("readonly") is not None, "the framework combobox accepts typed text")
    _clear_select(engine)
    report["last_step"] = step + "-required-engine-empty"
    _reject_field(dialog, form, imports, engine, form["engineRequired"], [reference, framework, framework_version])
    select(engine, engine_label)
    _clear_select(framework)
    report["last_step"] = step + "-required-framework-empty"
    _reject_field(dialog, form, imports, framework, form["frameworkRequired"], [reference, engine, framework_version])

    reference.fill("")
    framework_version.fill("")
    _clear_select(engine)
    report[step + "_required_fields"] = True


def exercise_archive_required(page, dialog, form, imports, report, base):
    """The archive URL row is one compact space, then each required field rejects empty and whitespace."""
    report["last_step"] = "archive-url-compact"
    compact = dialog.locator(".ant-space-compact")
    expect(compact).to_have_count(1)
    expect(compact).to_have_class(re.compile(r"\bant-space-compact\b"))
    expect(compact.locator("#image-archive-url")).to_have_count(1)
    report["archive_url_compact"] = True
    exercise_required_fields(page, dialog, form, imports, report, base, "archive")


def exercise_archive_minimum(page, server, text, form, source, report):
    """Create one custom image from the required fields only, then read it back."""
    report["last_step"] = "archive-minimum-create"
    ref, derived = "registry.example/team/minimum:v1.2.3", "team/minimum"
    button(page, text["page"]["addCustom"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    dialog.get_by_label(form["imageRef"], exact=True).fill(ref)
    options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    select(dialog.get_by_label(form["engine"], exact=True), next(item["label"] for item in options if item["name"] == "vllm"))
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    expect(dialog.get_by_label(form["seriesName"], exact=True)).to_have_value("")
    expect(dialog.get_by_label(form["engineVersion"], exact=True)).to_have_value("")
    rolling = dialog.get_by_role("switch")
    expect(rolling).to_have_count(1)
    expect(rolling).not_to_be_checked()
    expected = {"series": [{"engine": "vllm", "framework": "cuda", "source": source, "versions": [
        {"engine_version": "", "framework_version": "12.8", "arch": "multi", "image_ref": ref, "source": source,
         "rolling_tag": False, "registry_sources": []}]}]}
    import_path = "/api/v1/images/import"
    with page.expect_response(lambda response: urlsplit(response.url).path == import_path and response.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.status == 200, "minimum import failed")
    check(imported.value.request.post_data_json == expected, "minimum import body differs")
    created = collection(imported.value)
    check(len(created) == 1, "minimum import must create one series")
    series = created[0]
    check(series["name"] == derived and series["purpose"] == "inference" and series["enabled"] is True
          and series["engine"] == "vllm" and series["framework"] == "cuda" and series["source"] == source
          and series["present_nodes"] == 0, "minimum response differs")
    listed = collection(page.request.get(server.base + "/api/v1/images?source=" + source))
    check(any(row["id"] == series["id"] and row["name"] == derived for row in listed), "minimum series missing from the list")
    expect(button(page, derived)).to_be_visible()
    button(page, derived).click()
    details = page.get_by_role("dialog", name=f"{derived} vllm · cuda", exact=True)
    expect(details.get_by_role("link", name=ref, exact=True)).to_be_visible()
    detail = read_detail(page, server.base, series["id"])
    saved = detail["versions"][0]
    check(detail["name"] == derived and detail["purpose"] == "inference" and detail["enabled"] is True
          and detail["present_nodes"] == 0 and saved["image_ref"] == ref and saved["engine_version"] == "1.2.3"
          and saved["framework_version"] == "12.8" and saved["arch"] == "multi" and saved["rolling_tag"] is False
          and not saved.get("registry_sources"), "minimum detail differs")
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    expect(button(page, derived)).to_be_visible()
    button(page, derived).click()
    expect(page.get_by_role("dialog", name=f"{derived} vllm · cuda", exact=True).get_by_role("link", name=ref, exact=True)).to_be_visible()
    check(read_detail(page, server.base, series["id"]) == detail, "minimum reload differs")
    report["archive_minimum_create"] = True
    return series["id"]


def exercise_archive_full(page, server, text, form, source, report):
    """Create one custom image with every optional field set, then read it back."""
    report["last_step"] = "archive-full-create"
    series_name = "archive-full"
    ref = "registry.example/team/archive-full:v9.9.9"
    address = "https://containerimages.example/archive-full.tar"
    button(page, text["page"]["addCustom"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    dialog.get_by_label(form["seriesName"], exact=True).fill(series_name)
    dialog.get_by_label(form["imageRef"], exact=True).fill(ref)
    options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    select(dialog.get_by_label(form["engine"], exact=True), next(item["label"] for item in options if item["name"] == "vllm"))
    dialog.get_by_label(form["engineVersion"], exact=True).fill("9.9.9")
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    # The architecture select already shows its default, so the selection item
    # covers the combobox and a direct click never lands.
    arch = dialog.get_by_label(form["hostArch"], exact=True)
    arch.focus()
    arch.press("ArrowDown")
    expect(arch).to_have_attribute("aria-expanded", "true")
    page.get_by_text("amd64", exact=True).filter(visible=True).click()
    expect(arch).to_have_attribute("aria-expanded", "false")
    rolling = dialog.get_by_role("switch", name=form["rollingTag"], exact=True)
    expect(rolling).not_to_be_checked()
    rolling.click()
    expect(rolling).to_be_checked()
    sources = dialog.get_by_label(form["registrySources"], exact=True)
    sources.click()
    sources.fill(address)
    sources.press("Enter")
    expect(dialog.get_by_text(address, exact=True)).to_be_visible()
    expected = {"series": [{"name": series_name, "engine": "vllm", "framework": "cuda", "source": source, "versions": [
        {"engine_version": "9.9.9", "framework_version": "12.8", "arch": "amd64", "image_ref": ref, "source": source,
         "rolling_tag": True, "registry_sources": [address]}]}]}
    import_path = "/api/v1/images/import"
    with page.expect_response(lambda response: urlsplit(response.url).path == import_path and response.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.status == 200, "full import failed")
    check(imported.value.request.post_data_json == expected, "full import body differs")
    created = collection(imported.value)
    check(len(created) == 1, "full import must create one series")
    series = created[0]
    check(series["name"] == series_name and series["purpose"] == "inference" and series["enabled"] is True
          and series["engine"] == "vllm" and series["framework"] == "cuda" and series["source"] == source
          and series["present_nodes"] == 0, "full response differs")
    detail = read_detail(page, server.base, series["id"])
    saved = detail["versions"][0]
    check(detail["name"] == series_name and detail["purpose"] == "inference" and detail["enabled"] is True
          and detail["present_nodes"] == 0 and saved["image_ref"] == ref and saved["engine_version"] == "9.9.9"
          and saved["framework_version"] == "12.8" and saved["arch"] == "amd64" and saved["rolling_tag"] is True
          and saved.get("registry_sources") == [address], "full detail differs")
    listed = collection(page.request.get(server.base + "/api/v1/images?source=" + source))
    check(any(row["id"] == series["id"] and row["name"] == series_name for row in listed), "full series missing from the list")
    expect(button(page, series_name)).to_be_visible()
    button(page, series_name).click()
    opened = page.get_by_role("dialog", name=f"{series_name} vllm · cuda", exact=True)
    expect(opened.get_by_role("link", name=ref, exact=True)).to_be_visible()
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    expect(button(page, series_name)).to_be_visible()
    button(page, series_name).click()
    reopened = page.get_by_role("dialog", name=f"{series_name} vllm · cuda", exact=True)
    expect(reopened.get_by_role("link", name=ref, exact=True)).to_be_visible()
    check(read_detail(page, server.base, series["id"]) == detail, "full reload differs")
    reopened.locator(".ant-drawer-close").click()
    expect(reopened).to_be_hidden()
    report["archive_full_create"] = True
    return series["id"]


def exercise_community_minimum(page, server, text, form, source, report):
    """Create one community image from the required fields only, then read it back."""
    report["last_step"] = "community-minimum-create"
    ref, derived = "registry.example/team/minimum:v1.2.3", "team/minimum"
    button(page, text["page"]["addCommunity"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    dialog.get_by_label(form["imageRef"], exact=True).fill(ref)
    options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    select(dialog.get_by_label(form["engine"], exact=True), next(item["label"] for item in options if item["name"] == "vllm"))
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    expect(dialog.get_by_label(form["seriesName"], exact=True)).to_have_value("")
    expect(dialog.get_by_label(form["engineVersion"], exact=True)).to_have_value("")
    rolling = dialog.get_by_role("switch")
    expect(rolling).to_have_count(1)
    expect(rolling).not_to_be_checked()
    expected = {"series": [{"engine": "vllm", "framework": "cuda", "source": source, "versions": [
        {"engine_version": "", "framework_version": "12.8", "arch": "multi", "image_ref": ref, "source": source,
         "rolling_tag": False, "registry_sources": []}]}]}
    import_path = "/api/v1/images/import"
    with page.expect_response(lambda response: urlsplit(response.url).path == import_path and response.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.status == 200, "minimum import failed")
    check(imported.value.request.post_data_json == expected, "minimum import body differs")
    created = collection(imported.value)
    check(len(created) == 1, "minimum import must create one series")
    series = created[0]
    check(series["name"] == derived and series["purpose"] == "inference" and series["enabled"] is True
          and series["engine"] == "vllm" and series["framework"] == "cuda" and series["source"] == source
          and series["present_nodes"] == 0, "minimum response differs")
    listed = collection(page.request.get(server.base + "/api/v1/images?source=" + source))
    check(any(row["id"] == series["id"] and row["name"] == derived for row in listed), "minimum series missing from the list")
    expect(button(page, derived)).to_be_visible()
    button(page, derived).click()
    details = page.get_by_role("dialog", name=f"{derived} vllm · cuda", exact=True)
    expect(details.get_by_role("link", name=ref, exact=True)).to_be_visible()
    detail = read_detail(page, server.base, series["id"])
    saved = detail["versions"][0]
    check(detail["name"] == derived and detail["purpose"] == "inference" and detail["enabled"] is True
          and detail["present_nodes"] == 0 and saved["image_ref"] == ref and saved["engine_version"] == "1.2.3"
          and saved["framework_version"] == "12.8" and saved["arch"] == "multi" and saved["rolling_tag"] is False
          and not saved.get("registry_sources"), "minimum detail differs")
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    expect(button(page, derived)).to_be_visible()
    button(page, derived).click()
    expect(page.get_by_role("dialog", name=f"{derived} vllm · cuda", exact=True).get_by_role("link", name=ref, exact=True)).to_be_visible()
    check(read_detail(page, server.base, series["id"]) == detail, "minimum reload differs")
    report["community_minimum_create"] = True
    return series["id"]


def exercise_community_full(page, server, text, form, source, report):
    """Create one community image with every optional field set, then read it back."""
    report["last_step"] = "community-full-create"
    series_name = "community-full"
    ref = "registry.example/team/full:v9.9.9"
    address = "https://containerimages.example/full.tar"
    button(page, text["page"]["addCommunity"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_visible()
    dialog.get_by_label(form["seriesName"], exact=True).fill(series_name)
    dialog.get_by_label(form["imageRef"], exact=True).fill(ref)
    options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    select(dialog.get_by_label(form["engine"], exact=True), next(item["label"] for item in options if item["name"] == "vllm"))
    dialog.get_by_label(form["engineVersion"], exact=True).fill("9.9.9")
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    # The architecture select already shows its default, so the selection item
    # covers the combobox and a direct click never lands.
    arch = dialog.get_by_label(form["hostArch"], exact=True)
    arch.focus()
    arch.press("ArrowDown")
    expect(arch).to_have_attribute("aria-expanded", "true")
    page.get_by_text("amd64", exact=True).filter(visible=True).click()
    expect(arch).to_have_attribute("aria-expanded", "false")
    rolling = dialog.get_by_role("switch", name=form["rollingTag"], exact=True)
    expect(rolling).not_to_be_checked()
    rolling.click()
    expect(rolling).to_be_checked()
    sources = dialog.get_by_label(form["registrySources"], exact=True)
    sources.click()
    sources.fill(address)
    sources.press("Enter")
    expect(dialog.get_by_text(address, exact=True)).to_be_visible()
    expected = {"series": [{"name": series_name, "engine": "vllm", "framework": "cuda", "source": source, "versions": [
        {"engine_version": "9.9.9", "framework_version": "12.8", "arch": "amd64", "image_ref": ref, "source": source,
         "rolling_tag": True, "registry_sources": [address]}]}]}
    import_path = "/api/v1/images/import"
    with page.expect_response(lambda response: urlsplit(response.url).path == import_path and response.request.method == "POST") as imported:
        button(dialog, form["addImage"]).click()
    check(imported.value.status == 200, "full import failed")
    check(imported.value.request.post_data_json == expected, "full import body differs")
    created = collection(imported.value)
    check(len(created) == 1, "full import must create one series")
    series = created[0]
    check(series["name"] == series_name and series["purpose"] == "inference" and series["enabled"] is True
          and series["engine"] == "vllm" and series["framework"] == "cuda" and series["source"] == source
          and series["present_nodes"] == 0, "full response differs")
    detail = read_detail(page, server.base, series["id"])
    saved = detail["versions"][0]
    check(detail["name"] == series_name and detail["purpose"] == "inference" and detail["enabled"] is True
          and detail["present_nodes"] == 0 and saved["image_ref"] == ref and saved["engine_version"] == "9.9.9"
          and saved["framework_version"] == "12.8" and saved["arch"] == "amd64" and saved["rolling_tag"] is True
          and saved.get("registry_sources") == [address], "full detail differs")
    listed = collection(page.request.get(server.base + "/api/v1/images?source=" + source))
    check(any(row["id"] == series["id"] and row["name"] == series_name for row in listed), "full series missing from the list")
    expect(button(page, series_name)).to_be_visible()
    button(page, series_name).click()
    opened = page.get_by_role("dialog", name=f"{series_name} vllm · cuda", exact=True)
    expect(opened.get_by_role("link", name=ref, exact=True)).to_be_visible()
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    expect(button(page, series_name)).to_be_visible()
    button(page, series_name).click()
    reopened = page.get_by_role("dialog", name=f"{series_name} vllm · cuda", exact=True)
    expect(reopened.get_by_role("link", name=ref, exact=True)).to_be_visible()
    check(read_detail(page, server.base, series["id"]) == detail, "full reload differs")
    reopened.locator(".ant-drawer-close").click()
    expect(reopened).to_be_hidden()
    report["community_full_create"] = True
    return series["id"]


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
    report["last_step"] = "community-overview-statistics"
    stats = page.locator(".resource-summary-row .ant-statistic")
    expect(stats).to_have_count(5, timeout=12000)
    expect(stats.first).to_have_class(re.compile(r"\bant-statistic\b"))
    report["community_overview_statistics"] = True
    button(page, text["page"]["addCommunity"]).click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(button(dialog, fixture.repo_a)).to_be_visible()
    report["evidence"].append(assert_drawer_shell(
        page, dialog, form["title"][source],
        dialog.get_by_text(form["community"]["title"], exact=True),
        page.get_by_role("dialog", name=form["title"]["custom"], exact=True)))
    dialog.locator(".ant-drawer-close").focus()
    from image_community_inventory import exercise_community_inventory
    report["community_inventory"] = exercise_community_inventory(
        page, dialog, form, [fixture.repo_a, fixture.repo_b], imports)
    exercise_required_fields(page, dialog, form, imports, report, server.base, "community")
    engine_options = collection(page.request.get(server.base + "/api/v1/images/engines"))
    labels = {row["name"]: row["label"] for row in engine_options}
    with page.expect_response(lambda r: "/images/community/browse" in r.url) as first:
        button(dialog, fixture.repo_a).click()
    check(first.value.status == 200, "first community browse failed")
    a = first.value.json()
    check(parse_qs(urlsplit(first.value.url).query) == {"repo": [fixture.repo_a]}, "wrong repository query")
    check(a["engine"] == "vllm" and a["framework"] == "cuda", "catalog metadata lost")
    check({tag["tag"] for tag in a["tags"]} == {"nightly", "v1.2.3"}, "upstream tags differ")
    report["last_step"] = "community-tag-table"
    table = dialog.locator(".ant-table-small")
    expect(table).to_have_count(1)
    expect(table).to_have_class(re.compile(r"\bant-table-small\b"))
    report["community_tag_table"] = True
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
    reason = "upstream fixture is unreachable"
    held = []
    import_path = "/api/v1/images/import"
    import_route = re.compile(re.escape(import_path) + r"(?:\?|$)")

    def hold_import(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != import_path:
            route.continue_()
            return
        held.append(route)

    add = button(dialog, form["addImage"])
    page.route(import_route, hold_import)
    add.click()
    report["last_step"] = "community-import-inflight"
    expect(add).to_have_class(re.compile(r"\bant-btn-loading\b"))
    add.click()
    check(len(held) == 1, "duplicate image import while the first request is held")
    check(held[0].request.post_data_json == expected, "held import body differs")
    check(urlsplit(held[0].request.url).query == "", "unexpected import query")
    with page.expect_response(lambda r: urlsplit(r.url).path == import_path and r.request.method == "POST") as refused:
        held[0].fulfill(status=502, content_type="application/json", body='{"message":"%s"}' % reason)
    page.unroute(import_route, hold_import)
    check(refused.value.status == 502, "forced image import failure did not surface")
    expect(dialog).to_be_visible()
    expect(page.locator(".ant-message").get_by_text(reason, exact=True)).to_be_visible()
    expect(dialog.get_by_label(form["imageRef"], exact=True)).to_have_value(fixture.refs[1])
    expect(dialog.get_by_label(form["seriesName"], exact=True)).to_have_value(name)
    expect(dialog.get_by_label(form["frameworkVersion"], exact=True)).to_have_value("12.8")
    expect(dialog.get_by_label(form["engineVersion"], exact=True)).to_have_value(chosen["engine_version"])
    check(not any(row.get("name") == name for row in collection(page.request.get(server.base + "/api/v1/images"))), "refused import persisted a series")
    expect(add).not_to_have_class(re.compile(r"\bant-btn-loading\b"))
    with page.expect_response(lambda r: urlsplit(r.url).path == import_path and r.request.method == "POST") as imported:
        add.click()
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
    check(detail["enabled"] is True, "imported series is disabled")
    page.reload()
    page.get_by_role("tab", name=text["page"]["tab"][source], exact=True).click()
    report["last_step"] = "community-series-switch"
    series_switch = page.get_by_role(
        "switch", name=text["seriesTable"]["enabledLabel"].replace("{{name}}", name), exact=True)
    expect(series_switch).to_have_count(1, timeout=12000)
    expect(series_switch).to_have_class(re.compile(r"\bant-switch-small\b"))
    expect(series_switch).to_be_checked()
    report["community_series_switch"] = True
    button(page, name).click()
    # DetailDrawer names the dialog from the series plus its engine and framework.
    details = page.get_by_role("dialog", name=f"{name} sglang · cuda", exact=True)
    expect(details).to_be_in_viewport(ratio=1)
    expect(details.get_by_role("link", name=fixture.refs[1], exact=True)).to_be_visible()
    check(read_detail(page, server.base, result[0]["id"]) == detail, "refresh changed metadata")
    report["last_step"] = "community-reference-copy"
    details.get_by_role("button", name=text["nodeTable"]["expand"], exact=True).click()
    copy = details.locator(".ant-typography-copy")
    expect(copy).to_have_count(1, timeout=12000)
    expect(copy).to_have_class(re.compile(r"\bant-typography-copy\b"))
    expect(details.locator(".detail-extra").get_by_text(fixture.refs[1], exact=True)).to_be_visible()
    report["community_reference_copy"] = True
    check(all(re.fullmatch(r"Failed to load resource: the server responded with a status of 502 \(Bad Gateway\)", error["text"]) for error in console_errors), "unexpected console error")
    check(not errors and failures == [{"path": import_path, "status": 502}] and imports == [expected, expected], "unexpected browser error, HTTP failure or duplicate import")
    artifacts.write_json("browser-observations.json", {"errors": errors, "console_errors": console_errors, "failures": failures, "imports": imports})
    check([r["path"] for r in fixture.records] == ["/tags/a", "/tags/b"], "unexpected external registry access")
    report["evidence"].extend([{"layer": "ui", "selected": [fixture.repo_a + ":nightly", fixture.refs[1]], "manual_value_survived_search": True,
        "inflight_posts": 1, "confirm_loading": True, "failure_status": 502, "draft_retained": True, "failure_reason_visible": True},
        {"layer": "request", "status": imported.value.status, "body": expected, "browse": [a, b], "external_requests": fixture.records},
        {"layer": "persistence", "detail": detail, "reload": True}])
    details.locator(".ant-drawer-close").click()
    expect(details).to_be_hidden()
    full_id = exercise_community_full(page, server, text, form, source, report)
    minimum_id = exercise_community_minimum(page, server, text, form, source, report)
    cleanup(page, server.base, full_id, report)
    cleanup(page, server.base, minimum_id, report)
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
    report["last_step"] = "archive-add-icon"
    add_custom = button(page, text["page"]["addCustom"])
    expect(add_custom.locator(".ant-btn-icon")).to_have_count(1, timeout=12000)
    expect(add_custom.locator(".ant-btn-icon")).to_have_class(re.compile(r"\bant-btn-icon\b"))
    report["archive_add_icon"] = True
    add_custom.click()
    dialog = page.get_by_role("dialog")
    expect(dialog).to_be_in_viewport(ratio=1)
    report["last_step"] = "archive-registry-sources"
    multiple = dialog.locator(".ant-select-multiple")
    expect(multiple).to_have_count(1, timeout=12000)
    expect(multiple).to_have_class(re.compile(r"\bant-select-multiple\b"))
    report["archive_registry_sources"] = True
    report["evidence"].append(assert_drawer_shell(
        page, dialog, form["title"][source],
        dialog.get_by_text(form["url"]["title"], exact=True),
        dialog.get_by_text(form["community"]["title"], exact=True)))
    dialog.locator(".ant-drawer-close").focus()
    from image_archive_inventory import exercise_archive_inventory
    report["archive_inventory"] = exercise_archive_inventory(page, dialog, form, imports)
    exercise_archive_required(page, dialog, form, imports, report, server.base)
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
    held = []
    import_path = "/api/v1/images/import"
    import_route = re.compile(re.escape(import_path) + r"(?:\?|$)")

    def hold_import(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != import_path:
            route.continue_()
            return
        held.append(route)

    add = button(dialog, form["addImage"])
    page.route(import_route, hold_import)
    add.click()
    report["last_step"] = "archive-import-inflight"
    expect(add).to_have_class(re.compile(r"\bant-btn-loading\b"))
    add.click()
    check(len(held) == 1, "duplicate image import while the first request is held")
    check(held[0].request.post_data_json == expected, "held import body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == import_path and r.request.method == "POST") as imported:
        held[0].continue_()
    page.unroute(import_route, hold_import)
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
    # DetailDrawer names the dialog from the series plus its engine and framework.
    details = page.get_by_role("dialog", name=f"{name} vllm · cuda", exact=True)
    expect(details).to_be_in_viewport(ratio=1)
    expect(details.get_by_role("link", name=fixture.refs[1], exact=True)).to_be_visible()
    check(read_detail(page, server.base, result[0]["id"]) == detail, "archive reload differs")
    check(all(error["url"] == failed.value.url and re.fullmatch(r"Failed to load resource: the server responded with a status of 502 \(Bad Gateway\)", error["text"]) for error in console_errors), "unexpected console error")
    check(not errors and failures == [{"path": "/api/v1/images/preview-archive-url", "status": 502}] and imports == [expected], "unexpected browser error, HTTP failure or duplicate import")
    artifacts.write_json("browser-observations.json", {"errors": errors, "console_errors": console_errors, "failures": failures, "imports": imports})
    check(any(r["method"] == "HEAD" and r["status"] == 503 for r in fixture.records), "no real upstream failure")
    for path in ["/a.tar", "/b.tar?download=1"]:
        check(any(r["path"] == path and r["method"] == "GET" and r["range"] and r["status"] == 206 for r in fixture.records), "no real archive range read")
    report["evidence"].extend([{"layer": "ui", "manual_draft_retained_on_failure": True, "replacement": fixture.refs[1],
        "inflight_posts": 1, "confirm_loading": True, "failure_status": 502, "draft_retained": True,
        "failure_reason_visible": True},
        {"layer": "request", "body": expected, "status": imported.value.status, "preview": preview, "failed_status": 502, "external_requests": fixture.records},
        {"layer": "persistence", "detail": detail, "reload": True}])
    details.locator(".ant-drawer-close").click()
    expect(details).to_be_hidden()
    full_id = exercise_archive_full(page, server, text, form, source, report)
    minimum_id = exercise_archive_minimum(page, server, text, form, source, report)
    cleanup(page, server.base, full_id, report)
    cleanup(page, server.base, minimum_id, report)
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
                "ui/src/pages/images/ImageSeriesTable.tsx", "ui/src/pages/images/ImagesPageOverview.tsx",
                "ui/src/pages/images/ImageVersionColumns.tsx", "ui/src/pages/images/ImageImportDrawer.tsx",
                "test/e2e/image_archive_inventory.py", "test/e2e/image_community_inventory.py",
                "internal/server/api/images_catalog_import.go", "internal/server/api/images_community.go",
                "internal/server/api/images_archive_url.go"))
        finally:
            fixture.close()
            if previous is None:
                os.environ.pop("SSL_CERT_FILE", None)
            else:
                os.environ["SSL_CERT_FILE"] = previous
    raise SystemExit(result)
