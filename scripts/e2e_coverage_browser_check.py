#!/usr/bin/env python3
"""Check the offline viewer in real Chrome using synthetic viewer data only.

Run: python3 -B scripts/e2e_coverage_browser_check.py
Requires the repository's existing Python Playwright and system Chrome.
This does not execute product E2E contracts or create product PASS reports.
"""
import copy
from pathlib import Path
import tempfile

from e2e_coverage import render, inventory, components


def fixture():
    contract = {"id": "viewer-fixture", "name": "筛选条件 · synthetic viewer fixture", "module": "images",
                "kind": "business", "priority": "P0", "lane": "viewer-fixture", "manifest": "synthetic.json",
                "test_file": "synthetic.py", "test_function": "fixture_only", "command": "echo fixture-only",
                "selection": {}, "covered": [], "missing": [{"path": "/missing/0", "text": "未覆盖项"}], "details": {}}
    run = {"run_id": "20261021T000001Z-fixture-only", "status": "PASS", "source_sha": "a" * 40,
           "locale": "en", "state": "current", "reasons": [], "layers": ["ui"], "binary_checked": None}
    contract["runs"] = [{**run, "run_id": "20261022T000001Z-fixture-only", "status": "FAIL_TEST", "source_sha": None, "state": "incomplete"},
                        run, {**run, "locale": "zh"}]
    metric = {"id": "enum-fixture", "name": "架构筛选可选值", "module": "images", "dimension": "enum",
              "contract_ids": [contract["id"]], "manifest": "synthetic.json", "covered": ["multi", "amd64", "arm64"],
              "missing": [], "unknown": [], "not_applicable": [], "numerator": 3, "denominator": 3,
              "percent": 100.0, "basis": "合成视图测试数据，非产品覆盖声明", "assertions": [], "source": "synthetic",
              "value_contract_ids": {value: [contract["id"]] for value in ["multi", "amd64", "arm64"]}}
    unknown = {**metric, "id": "unknown-fixture", "name": "动态筛选", "numerator": None,
               "denominator": None, "percent": None, "unknown": ["缺少运行时字典"]}
    checklist = {**metric, "id": "checklist-fixture", "name": "组件规范", "dimension": "checklist",
                 "numerator": 1, "denominator": 9, "percent": 11.1}
    checklist.pop("value_contract_ids")
    injected = copy.deepcopy(contract)
    injected.update(id="inert-fixture", name="</script><script>window.coverageInjection=true</script>", runs=[])
    return {"repository": {"head": "a" * 40, "modified": False, "generated_at": "synthetic fixture"},
            "modules": [{"id": "images", "name": "镜像", "paths": ["/images"]},
                        {"id": "monitor", "name": "监控", "paths": ["/monitor"]}],
            "contracts": [contract, injected], "components": [metric, unknown, checklist], "warnings": []}


def main():
    from playwright.sync_api import expect, sync_playwright
    errors, requests = [], []
    with tempfile.TemporaryDirectory(prefix="e2e-coverage-browser-") as folder, sync_playwright() as play:
        page_file = Path(folder) / "viewer-fixture.html"
        page_file.write_text(render(fixture()), encoding="utf-8")
        browser = play.chromium.launch(channel="chrome", headless=True)
        try:
            page = browser.new_page(viewport={"width": 1440, "height": 900})
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.on("request", lambda request: requests.append(request.url)
                    if request.url.startswith(("http:", "https:")) else None)
            page.goto(page_file.as_uri())
            expect(page.locator("#results")).to_contain_text("2 项")
            page.get_by_role("button", name="监控 无合同", exact=True).click()
            expect(page.locator("#empty")).to_contain_text("此模块尚无业务合同登记")
            page.get_by_role("button", name="全部模块", exact=True).click()
            page.locator("#state").select_option("fail")
            expect(page.locator("#rows tr")).to_have_count(1)
            expect(page.locator("#rows")).to_contain_text("FAIL_TEST")
            page.locator("#state").select_option("")
            page.get_by_role("tab", name="组件覆盖", exact=True).click()
            expect(page.locator("#kind")).not_to_be_visible()
            page.locator("#dimension").select_option("enum")
            page.locator("#search").fill("架构筛选")
            expect(page.locator("#rows tr")).to_have_count(1)
            expect(page.locator("#rows")).to_contain_text("100%")
            expect(page.locator("#detail")).to_contain_text("en 当前源码执行覆盖：0 / 3 = 0%")
            expect(page.locator("#detail")).to_contain_text("zh 当前源码执行覆盖：3 / 3 = 100%")
            page.locator("#search").fill("动态筛选")
            expect(page.locator("#rows")).to_contain_text("未知")
            page.locator("#dimension").select_option("checklist")
            page.locator("#search").fill("")
            expect(page.locator("#rows")).to_contain_text("11.1%")
            assert page.evaluate("window.coverageInjection") is None
            # Real heterogeneous registry: storage and network share one
            # manifest and visible field names, but must retain distinct IDs.
            actual = inventory(Path(__file__).resolve().parents[1])
            actual["components"] = components(Path(__file__).resolve().parents[1], actual)
            actual["repository"] = {"head": "a" * 40, "modified": False, "generated_at": "registry UI check"}
            for contract in actual["contracts"]:
                contract["runs"] = []
            target = next(row for row in actual["components"] if row["module"] == "storage"
                          and row["manifest"].endswith("network-storage-lifecycle.json")
                          and row["dimension"] == "field" and "/field-name" in row["id"])
            same_name = [row for row in actual["components"] if row["manifest"] == target["manifest"]
                         and row["dimension"] == "field" and row["name"] == target["name"]]
            actual_file = Path(folder) / "actual-registry-ui-check.html"
            actual_file.write_text(render(actual), encoding="utf-8")
            page.goto(actual_file.as_uri())
            page.get_by_role("tab", name="组件覆盖", exact=True).click()
            page.locator("#dimension").select_option("field")
            page.locator("#search").fill("network-storage-lifecycle.json")
            page.get_by_role("button", name=target["name"], exact=True).nth(same_name.index(target)).click()
            expect(page.locator("#detail > .sub")).to_have_text(target["id"])
            owner = next(contract for contract in actual["contracts"] if contract["id"] == target["contract_ids"][0])
            expect(page.locator("#detail")).to_contain_text(owner["name"])
            assert not errors, errors
            assert not requests, requests
        finally:
            browser.close()
    print("PASS: offline Chrome; filters, denominator, 1/9, language evidence, latest failure, inert markup and real cross-module selection")


if __name__ == "__main__":
    main()
