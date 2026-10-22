#!/usr/bin/env python3
"""Image registration and sibling deletion through the real browser and server.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Build and run: make test-e2e-image-register / test-e2e-image-version-delete
After building: python3 test/e2e/image_catalog.py --case register --locale en
Choices: --case register or delete-version; --locale en or zh.
Optional CLI: --server-bin <binary>, --artifact-dir <private-directory>.
When a proxy is configured, set NO_PROXY and no_proxy to 127.0.0.1,localhost.

Each case starts its own real server, fresh SQLite and Chrome; no agent, Docker
or control-plane mock. Only catalog metadata is tested, not image availability.
GET /images/{id} addresses a series; DELETE /images/{id} addresses a version.
The parent-series deletion at teardown is cleanup, not business Delete coverage.
Field/CRUD/dimension mappings and gaps: coverage/image-catalog-*.json.

Artifacts default to secrets/e2e/<runId>/ (report, trace, observations, redacted
server log; failure screenshot/ARIA only after login). Success deletes only the
owned series; finally stops the owned server. Failed SQLite is kept for diagnosis.
Remove the private run directory within 7 days; never upload unredacted state.
"""
from __future__ import annotations

import json
import re
from urllib.parse import urlsplit
from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import check, collection
from image_catalog_runner import run


def button(scope, label):
    # AntD icon names join the accessible name; match the exact visible action.
    text = r"\s*".join(label) if re.fullmatch(r"[\u3400-\u9fff]{2}", label) else re.escape(label)
    return scope.get_by_role("button").filter(has_text=re.compile("^" + text + "$"))


def strings(locale):
    return json.loads((ROOT / f"ui/src/i18n/locales/{locale}/images.json").read_text())


def select(control, label):
    control.click()
    expect(control).to_have_attribute("aria-expanded", "true")
    control.page.get_by_text(label, exact=True).click()
    expect(control).to_have_attribute("aria-expanded", "false")


def open_custom(page, text):
    expect(page.get_by_role("heading", name=text["page"]["title"], exact=True)).to_be_visible()
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()


def read_detail(page, base, series_id):
    response = page.request.get(f"{base}/api/v1/images/{series_id}")
    check(response.status == 200, "image series detail must return 200")
    body = response.json()
    check(body["id"] == series_id and isinstance(body["versions"], list), "invalid series detail envelope")
    return body


def cleanup(page, base, series_id, report):
    response = page.request.delete(f"{base}/api/v1/image-series/{series_id}")
    check(response.status == 204, "owned series cleanup failed")
    check(not any(s["id"] == series_id for s in collection(page.request.get(base + "/api/v1/images"))), "series leaked")
    check(not any(v["series_id"] == series_id for v in collection(page.request.get(base + "/api/v1/images/versions"))), "version leaked")
    report["evidence"].append({"layer": "cleanup", "series_id": series_id, "remaining_owned_series": 0, "remaining_owned_versions": 0})


def observe(page):
    errors, failures, mutations = [], [], []
    page.on("pageerror", lambda e: errors.append(str(e)))
    page.on("console", lambda m: errors.append(m.text) if m.type == "error" else None)
    page.on("response", lambda r: failures.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path})
        if r.method in {"POST", "PATCH", "PUT", "DELETE"} and urlsplit(r.url).path.startswith(("/api/v1/images", "/api/v1/image-series")) else None)
    return errors, failures, mutations


# Contract: gpuworks/image-catalog/tagged-registration
# Mainline: 空自定义目录 → UI拒绝空值/无tag/缺框架版本、取消，再登记带tag镜像 → 精确POST/独立GET/刷新证明系列与版本分别持久化；无拉取或运行时声明。
def test_image_registration(page, server, artifacts, report, locale):
    text = strings(locale)
    form = text["import"]
    name = "e2e-" + report["run_id"]
    ref = f"registry.example/{name}/runner:v1"
    errors, failures, mutations = observe(page)
    open_custom(page, text)
    check(collection(page.request.get(server.base + "/api/v1/images?source=custom")) == [], "custom fixture must start empty")
    create = button(page, text["page"]["addCustom"])
    report["active_evidence_layer"] = "ui"
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/engines") as engines_response:
        create.click()
    engines = collection(engines_response.value)
    engine = next(item for item in engines if item["name"] == "vllm")
    dialog = page.get_by_role("dialog", name=form["title"]["custom"], exact=True)
    expect(dialog).to_be_in_viewport(ratio=1)
    expect(dialog.get_by_role("textbox", name=form["seriesName"], exact=True)).to_have_value("")
    reference = dialog.get_by_label(form["imageRef"], exact=True)
    submit = button(dialog, form["addImage"])
    # Real Tab navigation must reach the scrollable drawer action and remain modal.
    submit_reached = False
    hidden_focus_stops = 0
    for _ in range(18):
        page.keyboard.press("Tab")
        focus = dialog.evaluate("""el => ({inside: el.contains(document.activeElement),
            tag: document.activeElement.tagName, role: document.activeElement.getAttribute('role'),
            hidden: document.activeElement.getAttribute('aria-hidden'),
            label: document.activeElement.getAttribute('aria-label')})""")
        report["last_tab_focus"] = focus
        # rc-drawer uses aria-hidden DIV sentinels outside role=dialog to wrap Tab.
        # They are not background controls; the next Tab must return to the dialog.
        hidden_focus_stops = 0 if focus["inside"] else hidden_focus_stops + 1
        check(focus["inside"] or (focus["hidden"] == "true" and focus["tag"] == "DIV"
              and focus["role"] is None and hidden_focus_stops <= 1), "Tab reached background content")
        if submit.evaluate("el => el === document.activeElement"):
            expect(submit).to_be_in_viewport(ratio=1)
            submit_reached = True
    check(submit_reached, "submit is not keyboard reachable")
    dialog.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(create).to_be_focused()
    check(not mutations, "cancel must not send a mutation")
    create.click()
    expect(reference).to_have_value("")
    for value, message in [("", form["imageRefRequired"]), ("   ", form["tagRequired"]),
                           (f"registry.example/{name}/runner", form["tagRequired"])]:
        reference.fill(value)
        submit.click()
        expect(dialog.get_by_text(message, exact=True)).to_be_visible()
        check(not mutations, "invalid reference sent an import")
    reference.fill("  " + ref + "  ")
    dialog.get_by_role("textbox", name=form["seriesName"], exact=True).fill(name)
    select(dialog.get_by_label(form["engine"], exact=True), engine["label"])
    select(dialog.get_by_label(form["framework"], exact=True), "CUDA")
    submit.click()
    expect(dialog.get_by_text(form["frameworkVersionRequired"], exact=True)).to_be_visible()
    check(not mutations, "missing framework version sent an import")
    dialog.get_by_label(form["frameworkVersion"], exact=True).fill("12.8")
    dialog.get_by_role("textbox", name=form["engineVersion"], exact=True).fill("0.10.2")
    expect(dialog.get_by_role("switch", name=form["rollingTag"], exact=True)).not_to_be_checked()
    expect(dialog.get_by_role("combobox", name=form["hostArch"], exact=True)).to_have_attribute("aria-expanded", "false")
    expect(dialog.get_by_text("multi", exact=True)).to_be_visible()
    report["active_evidence_layer"] = "request"
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images/import" and r.request.method == "POST") as imported:
        submit.click()
    response = imported.value
    expected = {"series": [{"name": name, "engine": "vllm", "framework": "cuda", "source": "custom", "versions": [{
        "engine_version": "0.10.2", "framework_version": "12.8", "arch": "multi", "image_ref": ref,
        "source": "custom", "rolling_tag": False, "registry_sources": []}]}]}
    check(urlsplit(response.url).query == "", "import query must be empty")
    check(response.request.post_data_json == expected, "import wire body differs")
    series = collection(response)
    check(len(series) == 1, "import must return one series")
    series_id = series[0]["id"]
    check(mutations == [{"method": "POST", "path": "/api/v1/images/import"}], "unexpected mutations")
    expect(dialog).not_to_be_visible()
    expect(button(page, name)).to_be_visible()
    report["evidence"].extend([{"layer": "ui", "invalid": ["empty", "whitespace", "missing tag", "missing framework version"],
        "cancel_no_mutation": True, "keyboard_submit_and_modal_focus": True, "engine_catalog": engines}, {"layer": "request", "method": "POST", "path": "/api/v1/images/import", "status": response.status, "body": expected}])
    report["active_evidence_layer"] = "persistence"
    detail = read_detail(page, server.base, series_id)
    check(detail["name"] == name and detail["engine"] == "vllm" and detail["framework"] == "cuda" and detail["source"] == "custom", "series persistence differs")
    check(len(detail["versions"]) == 1 and detail["version_count"] == 1, "version count differs")
    version = detail["versions"][0]
    for key, value in expected["series"][0]["versions"][0].items():
        check(version.get(key, [] if key == "registry_sources" else False if key == "rolling_tag" else None) == value, f"persisted version field {key} differs")
    check(version["series_id"] == series_id and version["id"] > 0, "version identity differs")
    check(detail["enabled"] and version["enabled"] and detail["purpose"] == version["purpose"] == "inference", "server defaults differ")
    check(version["present_nodes"] == 0 and version["total_nodes"] == 0, "metadata registration fabricated node availability")
    check([v for v in collection(page.request.get(server.base + "/api/v1/images/versions")) if v["id"] == version["id"]] == [version], "independent versions projection differs")
    page.reload()
    open_custom(page, text)
    button(page, name).click()
    details = page.get_by_role("dialog", name=text["detail"]["title"].replace("{{name}}", name), exact=True)
    expect(details).to_be_in_viewport(ratio=1)
    expect(details.get_by_role("link", name=ref, exact=True)).to_be_visible()
    check(read_detail(page, server.base, series_id) == detail, "reload changed saved metadata")
    details.press("Escape")
    expect(details).not_to_be_visible()
    expect(button(page, name)).to_be_focused()
    report["evidence"].append({"layer": "persistence", "series": detail, "version_collection_match": True, "reload": True})
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page overflows horizontally")
    check(not errors and not failures, "browser errors or unexpected HTTP failures")
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    report["active_evidence_layer"] = "cleanup"
    cleanup(page, server.base, series_id, report)


# Contract: gpuworks/image-catalog/delete-sibling-version
# Mainline: API准备同系列无引用A/B → UI取消无mutation，再确认DELETE版本A → 独立系列/版本GET和刷新证明A消失、B/父系列不变；父系列仅清理，不声明拉取/运行时效果。
def test_delete_sibling_version(page, server, artifacts, report, locale):
    text = strings(locale)
    name = "e2e-" + report["run_id"]
    refs = [f"registry.example/{name}/runner:{tag}" for tag in ["a", "b"]]
    payload = {"series": [{"name": name, "engine": "vllm", "framework": "cuda", "source": "custom", "versions": [
        {"image_ref": ref, "engine_version": f"0.10.{i}", "framework_version": "12.8", "arch": "multi", "source": "custom"}
        for i, ref in enumerate(refs)]}]}
    seeded = collection(page.request.post(server.base + "/api/v1/images/import", data=payload))
    check(len(seeded) == 1, "fixture must create exactly one series")
    series_id = seeded[0]["id"]
    before = read_detail(page, server.base, series_id)
    check(len(before["versions"]) == 2, "fixture must create two versions")
    original = {v["image_ref"]: v for v in before["versions"]}
    a_id = original[refs[0]]["id"]
    errors, failures, mutations = observe(page)
    page.reload()
    open_custom(page, text)
    trigger = button(page, name)
    trigger.click()
    dialog = page.get_by_role("dialog", name=text["detail"]["title"].replace("{{name}}", name), exact=True)
    expect(dialog).to_be_in_viewport(ratio=1)
    for ref in refs:
        expect(dialog.get_by_role("link", name=ref, exact=True)).to_be_visible()
    dialog.get_by_role("link", name=refs[0], exact=True).click()
    remove = button(dialog, text["detail"]["deleteVersion"])
    expect(remove).to_be_in_viewport(ratio=1)
    report["active_evidence_layer"] = "ui"
    remove.click()
    confirm = page.get_by_role("tooltip")
    expect(confirm.get_by_text(text["versionTable"]["deleteTitle"].replace("{{ref}}", refs[0]), exact=True)).to_be_visible()
    expect(confirm.get_by_text(text["versionTable"]["deleteDescription"], exact=True)).to_be_visible()
    button(confirm, text["common"]["cancel"]).click()
    expect(confirm).not_to_be_visible()
    check(not mutations and read_detail(page, server.base, series_id) == before, "cancel changed a version")
    remove.click()
    report["active_evidence_layer"] = "request"
    with page.expect_response(lambda r: urlsplit(r.url).path == f"/api/v1/images/{a_id}" and r.request.method == "DELETE") as deleted:
        button(confirm, text["common"]["delete"]).click()
    check(deleted.value.status == 204, "version delete must return 204")
    check(urlsplit(deleted.value.url).query == "", "delete query must be empty")
    check(deleted.value.request.post_data is None, "DELETE unexpectedly has a body")
    check(mutations == [{"method": "DELETE", "path": f"/api/v1/images/{a_id}"}], "unexpected version delete mutations")
    expect(dialog.get_by_role("link", name=refs[0], exact=True)).to_have_count(0)
    expect(dialog.get_by_role("link", name=refs[1], exact=True)).to_be_visible()
    report["evidence"].extend([{"layer": "ui", "cancel_no_mutation": True, "confirmation_ref": refs[0], "impact": text["versionTable"]["deleteDescription"]},
        {"layer": "request", "method": "DELETE", "path": f"/api/v1/images/{a_id}", "status": 204}])
    report["active_evidence_layer"] = "persistence"
    after = read_detail(page, server.base, series_id)
    check(after["id"] == before["id"] and after["name"] == name and after["version_count"] == 1, "parent series not preserved")
    check(after["versions"] == [original[refs[1]]], "sibling version changed or target remains")
    # Version absence must use the version collection, not the series detail route.
    versions = collection(page.request.get(server.base + "/api/v1/images/versions"))
    check(not any(v["id"] == a_id for v in versions), "deleted version remains in catalog")
    check([v for v in versions if v["series_id"] == series_id] == after["versions"], "versions list differs from parent detail")
    page.reload()
    open_custom(page, text)
    button(page, name).click()
    expect(dialog.get_by_role("link", name=refs[0], exact=True)).to_have_count(0)
    expect(dialog.get_by_role("link", name=refs[1], exact=True)).to_be_visible()
    check(read_detail(page, server.base, series_id) == after, "reload changed preserved sibling")
    dialog.press("Escape")
    expect(dialog).not_to_be_visible()
    expect(button(page, name)).to_be_focused()
    report["evidence"].append({"layer": "persistence", "before": before, "after": after, "reload": True,
        "note": "GET /images/{id} addresses series; deletion addresses versions. Absence is checked in /images/versions."})
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page overflows horizontally")
    check(not errors and not failures, "browser errors or unexpected HTTP failures")
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    report["active_evidence_layer"] = "cleanup"
    cleanup(page, server.base, series_id, report)


if __name__ == "__main__":
    raise SystemExit(run({"register": test_image_registration, "delete-version": test_delete_sibling_version}))
