#!/usr/bin/env python3
"""Real-Chrome acceptance for the inference catalog and Playground.

The caller provisions a running browser-e2e deployment from
test/e2e/testdata/browser-catalog.yaml before invoking this script.  This file
deliberately drives visible controls and the real server proxy; it does not
mock catalog or inference responses.
"""

from __future__ import annotations

import argparse
import re
from pathlib import Path

from playwright.sync_api import Page, expect, sync_playwright


def select_mode(page: Page, label: str) -> None:
    page.locator(".playground-mode-bar .ant-segmented-item").filter(has_text=label).click()


def login(page: Page, base_url: str, password: str) -> None:
    page.goto(f"{base_url}/inference-templates?organization=my-templates&engine=vllm&kind=text")
    page.locator('input[autocomplete="username"]').fill("admin")
    page.locator('input[autocomplete="current-password"]').fill(password)
    page.get_by_role("button", name="Sign In").click()
    expect(page.get_by_role("heading", name="Inference Templates")).to_be_visible()


def open_from_catalog(page: Page) -> None:
    expect(page).to_have_url(re.compile(r"organization=my-templates.*engine=vllm.*kind=text"))
    page.get_by_role("button", name="Browser E2E", exact=True).click()
    expect(page.get_by_role("heading", name="Browser E2E")).to_be_visible()
    expect(page.get_by_text("NVIDIA · 1 specs")).to_be_visible()
    page.get_by_role("button", name="Details", exact=True).click()
    page.get_by_role("tab", name="Usage").click()
    expect(page.get_by_role("button", name="Open in Playground")).to_have_count(7)
    page.get_by_role("button", name="Open in Playground").first.click()
    expect(page).to_have_url(re.compile(r"/playground/chat.*template_id=\d+"))


def chat(page: Page) -> None:
    expect(page.locator(".playground-composer")).to_be_visible()
    composer = page.get_by_role("textbox", name="Message Input")
    composer.fill("hello through the real browser")
    page.locator(".playground-composer button.ant-btn-primary").click()
    expect(page.get_by_text("hello from fake-engine")).to_be_visible(timeout=10_000)


def vector(page: Page) -> None:
    select_mode(page, "Vector & Rerank")
    expect(page.get_by_role("heading", name="Input")).to_be_visible()
    page.locator(".playground-operation-grid button.ant-btn-primary").click()
    expect(page.get_by_text("0.125", exact=False)).to_be_visible()
    page.locator(".playground-operation-toolbar .ant-segmented-item").filter(has_text="Rerank").click()
    page.locator(".playground-operation-grid button.ant-btn-primary").click()
    expect(page.get_by_text("relevance_score", exact=False)).to_be_visible()


def audio(page: Page, upload_file: Path) -> None:
    select_mode(page, "Audio")
    page.locator(".playground-audio-toolbar .ant-segmented-item").filter(has_text="Speech").click()
    page.locator(".playground-audio-input button.ant-btn-primary").click()
    expect(page.locator("audio")).to_be_visible()
    page.locator(".playground-audio-toolbar .ant-segmented-item").filter(has_text="Transcription").click()
    page.locator('input[type="file"]').set_input_files(str(upload_file))
    page.locator(".playground-audio-input > button.ant-btn-primary").click()
    expect(page.get_by_text("hello from fake-engine")).to_be_visible()


def image(page: Page) -> None:
    select_mode(page, "Image")
    page.locator(".playground-operation-grid button.ant-btn-primary").click()
    expect(page.locator("img.playground-generated-image")).to_be_visible()


def video(page: Page) -> None:
    select_mode(page, "Video")
    generate = page.locator(".playground-operation-grid button.ant-btn-primary")
    generate.click()
    expect(page.locator("video.playground-generated-video")).to_be_visible(timeout=10_000)
    expect(page.locator("video.playground-generated-video")).to_have_attribute("src", re.compile(r"^blob:"))
    expect(generate).to_be_enabled()
    expect(generate).not_to_have_class(re.compile(r"ant-btn-loading"))

    page.get_by_role("spinbutton").fill("60")
    generate.click()
    cancel = page.locator(".playground-operation-grid button.ant-btn-dangerous")
    expect(cancel).to_be_visible()
    cancel.click()
    expect(page.get_by_text("deleted", exact=True)).to_be_visible()

    page.get_by_role("spinbutton").fill("4")
    page.locator(".playground-operation-grid textarea").fill("fail requested")
    generate.click()
    expect(page.locator(".ant-message-error")).to_contain_text("Video generation ended with status failed")


def return_state(page: Page, base_url: str) -> None:
    page.goto(f"{base_url}/inference-templates/gpuworks/browser-e2e?from=organization%3Dmy-templates%26engine%3Dvllm%26kind%3Dtext")
    page.locator("button").filter(has_text="Back to List").click()
    expect(page).to_have_url(re.compile(r"organization=my-templates.*engine=vllm.*kind=text"))
    expect(page.get_by_role("button", name="Browser E2E", exact=True)).to_be_visible()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:8802")
    parser.add_argument("--password", default="password")
    parser.add_argument("--artifact-dir", type=Path, required=True)
    args = parser.parse_args()
    args.artifact_dir.mkdir(parents=True, exist_ok=True)
    upload_file = Path(__file__).parent / "testdata" / "fake-model" / "model.safetensors"

    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(channel="chrome", headless=True)
        context = browser.new_context(locale="en-US", viewport={"width": 1600, "height": 1000}, accept_downloads=True)
        context.add_init_script("localStorage.setItem('gpuworks.locale', 'en')")
        page = context.new_page()
        page.set_default_timeout(10_000)
        page.on("console", lambda message: print(f"browser console {message.type}: {message.text}") if message.type == "error" else None)
        try:
            login(page, args.base_url.rstrip("/"), args.password)
            open_from_catalog(page)
            chat(page)
            vector(page)
            audio(page, upload_file)
            image(page)
            video(page)
            return_state(page, args.base_url.rstrip("/"))
            page.screenshot(path=str(args.artifact_dir / "inference-catalog-playground.png"), full_page=True)
        except Exception:
            page.screenshot(path=str(args.artifact_dir / "failure.png"), full_page=True)
            print(f"failure URL: {page.url}")
            print((page.locator("body").inner_text())[:4_000])
            raise
        finally:
            context.close()
            browser.close()
    print("Chrome E2E passed: catalog state + chat/vector/audio/image/video")


if __name__ == "__main__":
    main()
