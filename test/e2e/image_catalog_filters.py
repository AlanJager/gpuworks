#!/usr/bin/env python3
"""Image series client-side filtering against a real server and SQLite.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-image-filters
After building: python3 test/e2e/image_catalog_filters.py --case filters --locale en
Use --locale zh for Chinese; optional --server-bin and --artifact-dir follow the
shared runner. Set NO_PROXY/no_proxy=127.0.0.1,localhost when using a proxy.

Fixtures import owned custom/community series through the real API. Filtering
uses the complete GET /images response locally: no invented filter query, no
management mock, no agent/GPU/container. Architecture belongs to versions and
has no series filter. This page has no keyword search. Artifacts, trace and
failure SQLite stay in ignored secrets/e2e/<runId>; stop the owned server and
remove private runs within 7 days. Failed resources remain only in that DB.
"""
from __future__ import annotations

import json
from pathlib import Path
import re
from urllib.parse import urlsplit
from playwright.sync_api import expect
from image_catalog import button, cleanup, observe, read_detail, strings
from image_catalog_runner import run
from local_browser_fixture import ROOT
from platform_token_read_revoke import check, collection


def filter_menu(page, title):
    header = page.get_by_role("columnheader", name=re.compile("^" + re.escape(title) + r"(?:\s|$)"))
    header.get_by_role("button", name="filter", exact=True).click()
    menu = page.get_by_role("menu").filter(has=page.get_by_role("checkbox"))
    expect(menu).to_be_visible()
    return menu


def select_filter(page, title, values, options, locale):
    menu = filter_menu(page, title)
    expect(menu.get_by_role("menuitem")).to_have_text(options)
    for label in options:
        checkbox = menu.get_by_role("menuitem", name=label, exact=True).get_by_role("checkbox")
        checkbox.set_checked(label in values)
        expect(checkbox).to_be_checked(checked=label in values)
    button(page, "确定" if locale == "zh" else "OK").click()
    expect(menu).not_to_be_visible()


def reset_filter(page, title, options, locale):
    menu = filter_menu(page, title)
    button(page, "重置" if locale == "zh" else "Reset").click()
    for label in options:
        expect(menu.get_by_role("menuitem", name=label, exact=True).get_by_role("checkbox")).not_to_be_checked()
    button(page, "确定" if locale == "zh" else "OK").click()
    expect(menu).not_to_be_visible()


# Contract: gpuworks/image-catalog/series-filter-matrix
# Mainline: API准备各引擎/框架及启用/禁用的自定义系列和社区对照 → UI逐项筛选、同列OR/跨列AND、空交集与重置 → 对照独立GET精确行集合/总数，刷新保留筛选，重进恢复默认；只读筛选不改持久化，不声明镜像运行时。
def test_image_series_filters(page, server, artifacts, report, locale):
    text = strings(locale)
    common = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/common.json").read_text())
    prefix = "e2e-" + report["run_id"] + "-"
    engines = sorted(item["name"] for item in collection(page.request.get(server.base + "/api/v1/images/engines")))
    check(len(engines) >= 2, "fixture needs two real engines for OR/empty intersection")
    frameworks = ["cann", "cuda", "dtk", "hggc", "metal", "rocm"]
    fixture = []
    # Each engine has a CUDA row, each framework a row under engine A. Engine B
    # thus has CUDA but no CANN: both individual facets match, their AND is empty.
    combinations = [(engine, "cuda") for engine in engines] + [(engines[0], f) for f in frameworks if f != "cuda"]
    for index, (engine, framework) in enumerate(combinations):
        fixture.append({"name": prefix + str(index), "engine": engine, "framework": framework,
            "source": "custom", "enabled": index % 2 == 0, "versions": [{
                "image_ref": f"registry.example/{prefix}runner:{index}", "framework_version": "1.0",
                "engine_version": "1.0", "arch": "multi", "source": "custom"}]})
    fixture.append({"name": prefix + "community", "engine": engines[0], "framework": "cuda", "source": "community",
        "versions": [{"image_ref": f"registry.example/{prefix}community:v1", "framework_version": "1.0",
            "engine_version": "1.0", "arch": "amd64", "source": "community"}]})
    seeded = collection(page.request.post(server.base + "/api/v1/images/import", data={"series": fixture}))
    check(len(seeded) == len(fixture), "fixture series count differs")
    all_before = collection(page.request.get(server.base + "/api/v1/images"))
    owned = [row for row in all_before if row["name"].startswith(prefix)]
    check(len(owned) == len(fixture), "independent GET lost fixture rows")
    expected_fixture = {row["name"]: row for row in fixture}
    for row in owned:
        source = expected_fixture[row["name"]]
        for key in ["engine", "framework", "source", "enabled"]:
            check(row[key] == source.get(key, True), f"fixture persistence differs for {key}")
    custom = [row for row in owned if row["source"] == "custom"]
    check(len(custom) <= 20, "fixture outgrew the unpaginated filter matrix; extend explicit page collection")
    details_before = {row["id"]: read_detail(page, server.base, row["id"]) for row in owned}
    errors, failures, mutations = observe(page)
    reads = []
    page.on("response", lambda r: reads.append(r) if urlsplit(r.url).path == "/api/v1/images" and r.request.method == "GET" else None)
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images" and r.request.method == "GET") as initial:
        page.reload()
    check(collection(initial.value) == all_before, "browser list differs from independent authority")
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    expect(page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True)).to_have_attribute("aria-selected", "true")
    rows = page.get_by_role("table").get_by_role("button", name=re.compile("^" + re.escape(prefix)))

    def assert_rows(expected, dimension):
        expected_names = sorted(row["name"] for row in expected)
        expect(rows).to_have_count(len(expected_names))
        actual = sorted(rows.all_text_contents())
        check(actual == expected_names, f"{dimension}: included or excluded rows differ")
        # AntD hides pagination when the filtered list is empty; explicit empty
        # state plus zero row identities are the zero-result count evidence.
        if expected:
            total = common["resourceTable"]["total"].replace("{{count}}", str(len(expected)))
            expect(page.get_by_text(total, exact=True)).to_be_visible()
        else:
            expect(page.get_by_text(common["dataTable"]["empty"], exact=True)).to_be_visible()
        report["evidence"].append({"layer": "ui", "dimension": dimension, "ids": sorted(r["id"] for r in expected), "names": actual, "total": len(expected)})

    assert_rows(custom, "default-custom")
    titles = text["seriesTable"]["column"]
    status = [text["common"]["enabled"], text["common"]["disabled"]]
    report["active_evidence_layer"] = "ui"
    # Every actually visible option is matched to the owned authoritative rows,
    # selected separately and reset; no representative-value shortcut.
    for field, options in [("engine", engines), ("framework", frameworks), ("enabled", status)]:
        title = titles["status" if field == "enabled" else field]
        for option in options:
            select_filter(page, title, [option], options, locale)
            expected = [r for r in custom if (r["enabled"] == (option == status[0]) if field == "enabled" else r[field] == option)]
            check(expected, f"fixture option {field}/{option} must independently match a row")
            assert_rows(expected, f"{field}/option:{option}")
            reset_filter(page, title, options, locale)
            assert_rows(custom, f"{field}/reset:{option}")
    select_filter(page, titles["engine"], engines[:2], engines, locale)
    assert_rows([r for r in custom if r["engine"] in engines[:2]], "engine/multi-option-OR")
    select_filter(page, titles["framework"], ["cuda"], frameworks, locale)
    assert_rows([r for r in custom if r["engine"] in engines[:2] and r["framework"] == "cuda"], "engine-framework/AND")
    select_filter(page, titles["status"], [status[0]], status, locale)
    expected = [r for r in custom if r["engine"] in engines[:2] and r["framework"] == "cuda" and r["enabled"]]
    assert_rows(expected, "engine-framework-enabled/AND")
    # A real query refresh must preserve the current local filters and read the
    # same authoritative rows, with no filter parameters invented on the wire.
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/images" and r.request.method == "GET") as refreshed:
        page.get_by_role("button", name=common["dataTable"]["refresh"], exact=True).click()
    check(collection(refreshed.value) == all_before, "refresh altered authority")
    assert_rows(expected, "manual-refresh/filters-preserved")
    reset_filter(page, titles["status"], status, locale)
    select_filter(page, titles["engine"], [engines[1]], engines, locale)
    select_filter(page, titles["framework"], ["cann"], frameworks, locale)
    assert_rows([], "engine-framework/individually-nonempty-empty-intersection")
    reset_filter(page, titles["framework"], frameworks, locale)
    assert_rows([r for r in custom if r["engine"] == engines[1]], "empty-intersection/recovery")
    reset_filter(page, titles["engine"], engines, locale)
    assert_rows(custom, "all-filters/reset")
    page.get_by_role("tab", name=text["page"]["tab"]["community"], exact=True).click()
    assert_rows([r for r in owned if r["source"] == "community"], "source/community-excludes-custom")
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    assert_rows(custom, "source/custom-excludes-community")
    # Re-entering the page resets ephemeral filters and the default source tab.
    select_filter(page, titles["framework"], ["cann"], frameworks, locale)
    page.reload()
    expect(page.get_by_role("tab", name=text["page"]["tab"]["official"], exact=True)).to_have_attribute("aria-selected", "true")
    expect(rows).to_have_count(0)
    page.get_by_role("tab", name=text["page"]["tab"]["custom"], exact=True).click()
    assert_rows(custom, "reload/local-filters-reset")
    for response in reads:
        check(response.status == 200 and urlsplit(response.url).query == "" and response.request.post_data is None,
              "client-side filters must read unmodified GET /images")
        check(collection(response) == all_before, "unexpected filtered server response")
    check(not mutations, "read-only list filters caused a mutation")
    report["evidence"].append({"layer": "request", "method": "GET", "path": "/api/v1/images", "query": "", "status": 200,
        "browser_reads": len(reads), "response_ids": sorted(r["id"] for r in all_before), "filtering": "client-side", "mutations": mutations})
    for row in owned:
        check(read_detail(page, server.base, row["id"]) == details_before[row["id"]], "filter changed persisted metadata")
    report["evidence"].append({"layer": "persistence", "unchanged_series_ids": sorted(details_before), "reload": True})
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "page horizontal overflow")
    check(not errors and not failures, "browser errors or unexpected HTTP failures")
    artifacts.write_json("browser-observations.json", {"errors": errors, "failures": failures, "mutations": mutations})
    from metadata_table_checks import exercise as table_pilot
    table_pilot(page, server, artifacts, report, locale, text, engines, frameworks, select_filter, reset_filter)
    report["active_evidence_layer"] = "cleanup"
    for row in reversed(owned):
        cleanup(page, server.base, row["id"], report)


if __name__ == "__main__":
    raise SystemExit(run({"filters": test_image_series_filters}, entry_path=Path(__file__), metadata_offline=True, product_sources=[
        "test/e2e/metadata_table_checks.py", "test/e2e/metadata_table_fixture.py",
        "test/e2e/image_catalog.py", "ui/src/pages/ImagesPage.tsx", "ui/src/pages/images/ImageSeriesTable.tsx",
        "ui/src/pages/images/ImagesPageOverview.tsx", "ui/src/components/resourceTable/ResourceTable.tsx",
        "ui/src/components/resourceTable/columnPrefs.ts", "ui/src/components/resourceTable/ResizableHeaderCell.tsx"]))
