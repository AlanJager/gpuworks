#!/usr/bin/env python3
"""Mainline: observe real in-flight copy, reload, open its storage with keyboard.

Contract: test/e2e/browser-e2e-testing-standard.md. Driven by the isolated L3
fixture, using real management routes, agent writes and Docker readiness checks.
Run GPUWORKS_COPY_BROWSER=1 GPUWORKS_COPY_BROWSER_OUTPUT=<absolute directory>
go test -tags=docker -v ./test/l3 -run '^TestDockerModelCopyReadinessAndMount$'.
Requires Python Playwright and Chrome. This read/navigation case does not claim
retry/stop browser coverage or real GPU inference.
"""
import argparse
import json
import os
from pathlib import Path
from playwright.sync_api import sync_playwright, expect


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", required=True)
    parser.add_argument("--deployment", type=int, required=True)
    parser.add_argument("--storage", type=int, required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    evidence = {"mainline": "model-copy-observation-storage-navigation", "status": "FAIL", "deployment": args.deployment, "storage": args.storage, "requests": []}
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(executable_path=os.environ.get("CHROME_PATH", "/usr/bin/google-chrome"), headless=True, args=["--no-sandbox"])
        context = browser.new_context(locale="en-US", timezone_id="UTC", viewport={"width": 1440, "height": 900})
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        detail = f"/api/v1/deployments/{args.deployment}"
        page.on("response", lambda response: evidence["requests"].append({"method": response.request.method, "path": response.url.removeprefix(args.base), "status": response.status}) if response.url.endswith(detail) or response.url.endswith(f"/api/v1/storages/{args.storage}?all_nodes=1") else None)
        try:
            page.goto(args.base + "/deployments")
            page.locator('input[autocomplete="username"]').fill("admin")
            page.locator('input[autocomplete="current-password"]').fill("password")
            page.get_by_role("button", name="Sign In").click()
            expect(page.get_by_role("heading", name="Deployments", exact=True)).to_be_visible()
            with page.expect_response(lambda response: response.url.endswith(detail) and response.status == 200) as received:
                page.goto(args.base + f"/deployments?deployment={args.deployment}")
            data = received.value.json()
            row = next(row for row in data["model_progress"] if row["storage_id"] == args.storage)
            assert row["preparation"]["phase"] == "copying", row
            assert 0 < row["downloaded"] < row["size_bytes"], row
            assert not data.get("instances"), data.get("instances")
            panel = page.get_by_role("region", name="Model Preparation")
            expect(panel).to_be_visible()
            expect(panel.get_by_text("Copying to local cache", exact=True)).to_be_visible()
            expect(panel.get_by_text("copy-model", exact=True)).to_be_visible()
            initial = panel.locator(".ant-progress-text").inner_text()
            page.wait_for_function("initial => { const e = document.querySelector('section[aria-label=\"Model Preparation\"] .ant-progress-text'); return e && e.textContent !== initial; }", arg=initial, timeout=15000)
            page.screenshot(path=str(output / "copy-progress.png"), full_page=True)
            with page.expect_response(lambda response: response.url.endswith(detail) and response.status == 200) as reloaded:
                page.reload()
            current = reloaded.value.json()
            refreshed = next(row for row in current["model_progress"] if row["storage_id"] == args.storage)
            assert refreshed["downloaded"] >= row["downloaded"]
            check = context.request.get(args.base + detail)
            assert check.status == 200
            assert next(row for row in check.json()["model_progress"] if row["storage_id"] == args.storage)["downloaded"] >= refreshed["downloaded"]
            storage = panel.get_by_role("link", name="copy-target")
            storage.focus()
            expect(storage).to_be_focused()
            with page.expect_response(lambda response: response.url.endswith(f"/api/v1/storages/{args.storage}?all_nodes=1") and response.status == 200):
                storage.press("Enter")
            expect(page).to_have_url(args.base + f"/storage/{args.storage}")
            page.screenshot(path=str(output / "copy-storage.png"), full_page=True)
            assert not errors, errors
            evidence.update(status="PASS", browser=browser.version, locale="en-US", timezone="UTC", initial_bytes=row["downloaded"], refreshed_bytes=refreshed["downloaded"], total_bytes=row["size_bytes"])
        except Exception as error:
            evidence["error"] = str(error)
            page.screenshot(path=str(output / "failure.png"), full_page=True)
            raise
        finally:
            (output / "browser.json").write_text(json.dumps(evidence, indent=2))
            browser.close()


if __name__ == "__main__":
    main()
