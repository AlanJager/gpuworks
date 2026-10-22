#!/usr/bin/env python3
"""Run supported manifest navigation assertions or existing-deployment renderer smoke.

E2E standard: test/e2e/browser-e2e-testing-standard.md
The historical GPU matrix includes unimplemented actions/expectations and fails
closed with a MISSING report before browser startup. No cases are removed.
--existing-deployment is a separate fake-engine renderer smoke lane, not full
Playground, real-model inference or manifest business coverage.

Run: python3 test/e2e/playground_acceptance.py --cases <manifest> --artifact-dir <new-directory>
Tests: python3 -m unittest discover -s test/e2e -p test_playground_acceptance.py
"""
from __future__ import annotations
import argparse, json, re, shutil, sys, time
from pathlib import Path
from typing import Any
from urllib.parse import urlencode
from uuid import uuid4
import yaml
from playwright.sync_api import Page, expect, sync_playwright
from playground_manifest import ManifestLoader, NAVIGATION_PAGES, case_gaps, validate_manifest


def select_playground_mode(page: Page, label: str) -> None:
    page.locator(".playground-mode-bar .ant-segmented-item").filter(has_text=label).click()


def check_existing_deployment(page: Page, base: str, deployment_name: str, model: str, audio_file: Path | None) -> list[dict[str, Any]]:
    """Mainline: existing fake-engine → renderer smoke; optional transcription, no GPU claim."""
    observations: list[dict[str, Any]] = []
    speech_requests: list[str] = []
    page.on("request", lambda request: speech_requests.append(request.post_data or "") if request.url.endswith("/v1/audio/speech") else None)

    page.goto(base + "/deployments")
    row = page.locator("tr").filter(has_text=deployment_name)
    expect(row).to_be_visible()
    row.get_by_role("button", name="Open Playground").click()
    expect(page).to_have_url(re.compile(rf"/playground/chat\?operation=openai\.chat&model={re.escape(model)}"))
    observations.append({"route": page.url})

    composer = page.get_by_role("textbox", name="Message Input")
    composer.fill("hello from real GPU acceptance")
    page.locator(".playground-composer button.ant-btn-primary").click()
    expect(page.get_by_text("hello from fake-engine")).to_be_visible()
    observations.append({"chat": "ok"})

    select_playground_mode(page, "Vector & Rerank")
    expect(page.get_by_role("heading", name="Input")).to_be_visible()
    page.locator(".playground-operation-grid button.ant-btn-primary").click()
    expect(page.get_by_text("0.125", exact=False)).to_be_visible()
    page.locator(".playground-operation-toolbar .ant-segmented-item").filter(has_text="Rerank").click()
    page.locator(".playground-operation-grid button.ant-btn-primary").click()
    expect(page.get_by_text("relevance_score", exact=False)).to_be_visible()
    observations.append({"embeddings": "ok", "rerank": "ok"})

    select_playground_mode(page, "Audio")
    page.locator(".playground-audio-toolbar .ant-segmented-item").filter(has_text="Speech").click()
    page.get_by_role("button", name="Request").click()
    expect(page.get_by_text("/v1/audio/speech")).to_be_visible()
    page.get_by_role("button", name="OpenAPI skeleton").click()
    editor = page.get_by_role("textbox", name="Raw request body")
    expect(editor).to_have_value(re.compile(rf'"model": "{re.escape(model)}"'))
    generated = editor.input_value()
    page.get_by_role("button", name="Save preset").click()
    editor.fill('{}')
    page.get_by_role("button", name="Load preset").click()
    expect(editor).to_have_value(generated)
    page.get_by_role("button", name="Send Request").click()
    expect(page.locator("audio")).to_be_visible()
    if not speech_requests or json.loads(speech_requests[-1]).get("model") != model:
        raise AssertionError(f"expert speech request was not forwarded unchanged: {speech_requests}")
    observations.append({"speech": "ok", "expert_raw": "ok", "openapi_skeleton": "ok", "preset": "ok"})

    if audio_file is not None:
        page.locator(".playground-audio-toolbar .ant-segmented-item").filter(has_text="Transcription").click()
        page.locator('input[type="file"]').set_input_files(str(audio_file))
        page.locator(".playground-audio-input > button.ant-btn-primary").click()
        expect(page.get_by_text("hello from fake-engine")).to_be_visible()
        observations.append({"transcription": "ok"})

    select_playground_mode(page, "Image")
    page.locator(".playground-operation-grid button.ant-btn-primary").click()
    expect(page.locator("img.playground-generated-image")).to_be_visible()
    observations.append({"image": "ok"})

    select_playground_mode(page, "Video")
    page.locator(".playground-operation-grid button.ant-btn-primary").click()
    expect(page.locator("video.playground-generated-video")).to_be_visible(timeout=15_000)
    observations.append({"video": "ok"})

    select_playground_mode(page, "Realtime")
    page.get_by_role("button", name="Connect").click()
    expect(page.locator(".playground-realtime-log")).to_contain_text("session.created")
    page.get_by_role("button", name="Send Event").click()
    expect(page.locator(".playground-realtime-log")).to_contain_text("session.updated")
    observations.append({"realtime": "ok"})
    return observations


def login(page: Page, base: str, username: str, password: str) -> None:
    page.goto(base + "/login")
    page.locator('input[autocomplete="username"]').fill(username)
    page.locator('input[autocomplete="current-password"]').fill(password)
    page.get_by_role("button", name=re.compile("sign in", re.I)).click()
    expect(page).not_to_have_url(re.compile(r"/login$"))


def deploy(page: Page, base: str, item: dict[str, Any], node_uuid: str | None) -> int:
    deploy = item.get("deploy") or {}
    template = deploy.get("template")
    spec = deploy.get("spec")
    templates_response = page.request.get(base + "/api/v1/inference-templates")
    if not templates_response.ok: raise RuntimeError(f"cannot list inference templates: {templates_response.status}")
    templates = templates_response.json()
    if isinstance(templates, dict): templates = templates.get("items", templates.get("templates", []))
    match = next((item for item in templates if item.get("model_id") == template or item.get("model") == template), None)
    if not match: raise RuntimeError(f"template {template!r} was not found")
    spec_match = next((item for item in match.get("specs", []) if item.get("name") == spec), None)
    if not spec_match: raise RuntimeError(f"spec {spec!r} for {template!r} was not found")
    params = {"model_id": match.get("id", match.get("model_id")), "template_id": match.get("id"), "template_spec_source": spec_match.get("source", "upstream"), "template_spec_name": spec, "served_name": deploy.get("served_name"), "node_uuid": node_uuid, "engine": spec_match.get("engine", "vllm"), "name": deploy.get("served_name", template)}
    params = {key: value for key, value in params.items() if value not in (None, "")}
    response = page.request.post(base + "/api/v1/deployments", data=params)
    if not response.ok:
        raise RuntimeError(f"deployment admission failed: {response.status} {response.text()}")
    return response.json()["id"]


def wait_running(page: Page, base: str, deployment_id: int, timeout: float) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        response = page.request.get(base + f"/api/v1/deployments/{deployment_id}")
        if response.ok:
            value = response.json()
            if value.get("runtime_state") == "running": return value
            if value.get("runtime_state") == "failed": raise RuntimeError(value)
        time.sleep(2)
    raise TimeoutError(f"deployment {deployment_id} did not become running")


def check_case(page: Page, base: str, case: dict[str, Any], deployment: dict[str, Any] | None) -> dict[str, Any]:
    """Mainline: explicit navigation → URL/visible-text assertions, without inference claims."""
    gaps = case_gaps(case)
    if gaps:
        raise ValueError('MISSING: ' + '; '.join(gaps))
    browser = case.get("browser", "")
    result: dict[str, Any] = {"id": case["id"], "browser": browser, "url": page.url, "scope": "navigation-only"}
    if browser == "open_unsupported_chat":
        page.goto(base + '/playground/chat?' + urlencode({'model': case['reuse']}))
        expect(page.get_by_text(re.compile("does not declare", re.I))).to_be_visible()
    elif browser == "speech_form":
        page.goto(base + '/playground/audio?' + urlencode({'operation': 'openai.audio.speech', 'model': case['reuse']}))
        expect(page.get_by_role("button", name=re.compile("request", re.I))).to_be_visible()
    elif browser in NAVIGATION_PAGES:
        page.goto(base + '/playground/' + browser)
    result["url"] = page.url
    expected = case.get("expect", {})
    if expected.get("url_regex"): expect(page).to_have_url(re.compile(expected["url_regex"]))
    for text in expected.get("visible_text", []): expect(page.get_by_text(text, exact=False)).to_be_visible()
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--cases", default="test/e2e/testdata/playground-acceptance/cases.yaml")
    parser.add_argument("--base-url")
    parser.add_argument("--password")
    parser.add_argument("--keep-failed", action="store_true")
    parser.add_argument("--existing-deployment", help="renderer smoke against an already running fake-engine deployment")
    parser.add_argument("--existing-model", help="served model name used with --existing-deployment")
    parser.add_argument("--audio-file", type=Path, help="optional audio fixture for the transcription renderer")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--chrome-executable", default=shutil.which("google-chrome"))
    args = parser.parse_args()
    try:
        manifest = yaml.load(Path(args.cases).read_text(), Loader=ManifestLoader)
    except yaml.YAMLError:
        # Do not print parser excerpts: manifests may contain credentials.
        manifest = None
    if not args.existing_deployment:
        preflight = validate_manifest(manifest)
        if preflight['status'] == 'MISSING':
            artifact = args.artifact_dir or Path(__file__).resolve().parents[2] / 'secrets/e2e' / ('playground-preflight-' + uuid4().hex)
            artifact.mkdir(parents=True, exist_ok=True)
            (artifact / 'report.json').write_text(json.dumps(preflight, ensure_ascii=False, indent=2))
            print('MISSING: manifest has unsupported or invalid semantics; no external actions started. Report: ' + str(artifact / 'report.json'), file=sys.stderr)
            return 2
    else:
        if not args.existing_model:
            parser.error('--existing-model is required with --existing-deployment')
        if not isinstance(manifest, dict) or not isinstance(manifest.get('defaults', {}), dict):
            parser.error('smoke defaults must be a YAML object')
    defaults = manifest.get('defaults', {})
    base = args.base_url or defaults.get('base_url')
    if not base:
        parser.error('explicit --base-url or defaults.base_url is required')
    artifact = args.artifact_dir or Path(defaults.get("artifact_dir", "artifacts/archive/2026-08-21-playground/playground-request-composition-gpu3")) / time.strftime("%Y-%m-%d")
    artifact.mkdir(parents=True, exist_ok=True); report: list[dict[str, Any]] = []; active: dict[int, bool] = {}
    failed = False
    with sync_playwright() as playwright:
        launch = {"headless": True}
        if args.chrome_executable:
            launch["executable_path"] = args.chrome_executable
            launch["args"] = ["--no-sandbox"]
        browser = playwright.chromium.launch(**launch)
        context = browser.new_context(locale="en-US", viewport={"width": 1600, "height": 1000})
        context.add_init_script("localStorage.setItem('gpuworks.locale', 'en')")
        page = context.new_page()
        page.set_default_timeout(15_000)
        login(page, base, defaults.get("username", "admin"), args.password or defaults.get("password", "password"))
        try:
            if args.existing_deployment:
                report.append({'scope': 'existing-deployment-renderer-smoke',
                    'not_claimed': ['manifest business cases', 'real-model inference', 'GPU behavior']})
                report.extend(check_existing_deployment(page, base, args.existing_deployment, args.existing_model, args.audio_file))
                page.screenshot(path=str(artifact / "playground-success.png"), full_page=True)
                return 0
            for case in manifest["cases"]:
                deployment_id = None
                try:
                    deployment_id = deploy(page, base, case, defaults.get("node_uuid")) if case.get("deploy") else None
                    if deployment_id: active[deployment_id] = True; deployment = wait_running(page, base, deployment_id, defaults.get("ready_timeout_seconds", 1800))
                    else: deployment = None
                    report.append(check_case(page, base, case, deployment))
                    if deployment_id and not (case.get("deploy") or {}).get("keep"): page.request.post(base + f"/api/v1/deployments/{deployment_id}/stop"); active.pop(deployment_id, None)
                except Exception as error:
                    failed = True
                    report.append({"id": case["id"], "error": str(error), "url": page.url}); page.screenshot(path=str(artifact / (case["id"] + ".png")), full_page=True); raise
        finally:
            (artifact / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
            if not failed and not args.keep_failed:
                for deployment_id in active: page.request.post(base + f"/api/v1/deployments/{deployment_id}/stop")
            context.close()
            browser.close()
    return 0

if __name__ == "__main__": sys.exit(main())
