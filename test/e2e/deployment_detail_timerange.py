#!/usr/bin/env python3
"""Real-Chrome acceptance for the deployment-detail time-range controls.

The caller provisions a running deployment (fake-engine is enough: the range
controls must exist and must drive the metrics query parameters even while the
engine reports no points). Explicit gpu3/L4 manual job only, never the offline
unit suite.

Verified behavior:
- the deployment drawer shows the time-range select over the metrics panel;
- quick ranges re-issue /api/v1/metrics/query with start/end/step for the window;
- the custom range drives start/end through the RangePicker;
- a range beyond retention surfaces the warning;
- the instance log tab's range picker queries /api/v1/logs/search with bounds.

The absence of the chart dataZoom slider is asserted by the offline unit test
(buildTimeSeriesOption); canvas rendering keeps it opaque to DOM queries here.
"""
from __future__ import annotations

import argparse
import re
import time
from pathlib import Path
from urllib.parse import urlparse

from playwright.sync_api import Page, expect


def wait_for(predicate, timeout_ms: int, message: str) -> None:
    deadline = time.time() + timeout_ms / 1000
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(0.2)
    raise AssertionError(f"timeout waiting for: {message}")


def login(page: Page, base_url: str, password: str) -> None:
    page.goto(f"{base_url}/deployments")
    page.locator('input[autocomplete="username"]').fill("admin")
    page.locator('input[autocomplete="current-password"]').fill(password)
    page.get_by_role("button", name="Sign In").click()
    expect(page.get_by_role("heading", name="Deployments")).to_be_visible()


def open_drawer(page: Page, base_url: str, deployment_id: int) -> None:
    page.goto(f"{base_url}/deployments?deployment={deployment_id}")
    expect(page.get_by_role("heading", name="Deployment Details")).to_be_visible()


def track_requests(page: Page, fragment: str) -> list[str]:
    urls: list[str] = []
    page.on(
        "request",
        lambda request: urls.append(request.url) if fragment in request.url else None,
    )
    return urls


def query_params(url: str) -> dict[str, str]:
    return dict(pair.split("=", 1) for pair in urlparse(url).query.split("&") if "=" in pair)


def choose_range(page: Page, label: str, index: int = 0) -> None:
    drawer = page.locator(".ant-drawer-content-wrapper")
    drawer.get_by_label("Time range").nth(index).click()
    page.locator(".ant-select-dropdown:visible .ant-select-item-option-content").filter(
        has_text=label
    ).first.click()


def quick_range_drives_metrics_queries(page: Page) -> None:
    urls = track_requests(page, "/api/v1/metrics/query")
    drawer = page.locator(".ant-drawer-content-wrapper")
    expect(drawer.get_by_label("Time range").first).to_be_visible()

    def saw_windowed_query() -> bool:
        return any(
            query_params(url).get("step") == "10s"
            and query_params(url).get("start")
            and query_params(url).get("end")
            for url in urls
        )

    wait_for(saw_windowed_query, 10_000, "default 15m metrics query with start/end/step")

    choose_range(page, "1 hour")
    wait_for(
        lambda: any(query_params(url).get("step") == "30s" for url in urls),
        10_000,
        "1 hour metrics query carries step=30s",
    )


def custom_range_drives_start_end(page: Page) -> None:
    urls = track_requests(page, "/api/v1/metrics/query")
    drawer = page.locator(".ant-drawer-content-wrapper")
    choose_range(page, "Custom")
    inputs = drawer.locator(".ant-picker:visible input")
    expect(inputs.first).to_be_visible()
    inputs.first.fill("2026-09-12 22:00:00")
    inputs.first.press("Tab")
    inputs.nth(1).fill("2026-09-19 22:00:00")
    inputs.nth(1).press("Enter")

    # A 7-day window resolves to the 1-week bucket step (1h); the bounds pass
    # through verbatim so the server never has to guess the anchor.
    wait_for(
        lambda: any(
            query_params(url).get("step") == "1h"
            and query_params(url).get("start")
            and query_params(url).get("end")
            for url in urls
        ),
        10_000,
        "custom range metrics query carries start/end/step=1h",
    )


def beyond_retention_warns(page: Page) -> None:
    choose_range(page, "1 month")
    expect(page.get_by_text(re.compile("exceeds the 7-day retention"))).to_be_visible()
    choose_range(page, "15 minutes")
    expect(page.get_by_text(re.compile("exceeds the 7-day retention"))).to_have_count(0)


def log_range_queries_store(page: Page) -> None:
    searches = track_requests(page, "/api/v1/logs/search")
    drawer = page.locator(".ant-drawer-content-wrapper")
    drawer.get_by_role("tab", name="Logs").click()
    choose_range(page, "6 hours", index=1)
    wait_for(
        lambda: any(
            query_params(url).get("start") and query_params(url).get("end")
            for url in searches
        ),
        10_000,
        "log history query carries start/end",
    )
    expect(drawer.get_by_text(re.compile("persisted logs in the selected range"))).to_be_visible()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:8802")
    parser.add_argument("--password", default="password")
    parser.add_argument("--deployment-id", type=int, required=True, help="an existing deployment whose drawer is inspected")
    parser.add_argument("--artifact-dir", type=Path, required=True)
    args = parser.parse_args()
    args.artifact_dir.mkdir(parents=True, exist_ok=True)

    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(channel="chrome", headless=True)
        context = browser.new_context(locale="en-US", viewport={"width": 1600, "height": 1000})
        context.add_init_script("localStorage.setItem('gpuworks.locale', 'en')")
        page = context.new_page()
        page.set_default_timeout(10_000)
        try:
            login(page, args.base_url.rstrip("/"), args.password)
            open_drawer(page, args.base_url.rstrip("/"), args.deployment_id)
            quick_range_drives_metrics_queries(page)
            custom_range_drives_start_end(page)
            beyond_retention_warns(page)
            log_range_queries_store(page)
            page.screenshot(path=str(args.artifact_dir / "deployment-detail-time-range.png"), full_page=True)
        except Exception:
            page.screenshot(path=str(args.artifact_dir / "failure.png"), full_page=True)
            print(f"failure URL: {page.url}")
            print((page.locator("body").inner_text())[:4_000])
            raise
        finally:
            context.close()
            browser.close()
    print("Chrome E2E passed: deployment detail time-range controls")


if __name__ == "__main__":
    main()
