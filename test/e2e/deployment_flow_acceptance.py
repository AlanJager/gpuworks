#!/usr/bin/env python3
"""Chrome acceptance of the real deployment form against a prepared fake node.

Run through deployment_flow_env.py. Network responses are never mocked: the
fixture replaces only physical GPU facts and container execution. A failed
deployment remains in the isolated database for inspection.
"""

from __future__ import annotations

import argparse
import json
import re
import time
import traceback
from pathlib import Path

from playwright.sync_api import Page, expect, sync_playwright


class Evidence:
    def __init__(self) -> None:
        self.calls: list[dict] = []
        self.console_errors: list[str] = []
        self.steps: list[str] = []

    def watch(self, page: Page) -> None:
        def response_ready(response) -> None:
            path = response.url.split("?", 1)[0]
            if not (path.endswith("/api/v1/evaluate") or path.endswith("/api/v1/deployments")):
                return
            request = response.request
            if request.method != "POST":
                return
            try:
                body = request.post_data_json
            except Exception:
                body = request.post_data
            try:
                result = response.json()
            except Exception:
                result = response.text()[:2000]
            self.calls.append({"path": path.rsplit("/", 1)[-1], "status": response.status,
                "request": body, "response": result})
        page.on("response", response_ready)
        page.on("console", lambda message: self.console_errors.append(message.text)
            if message.type == "error" else None)
        page.on("pageerror", lambda error: self.console_errors.append(str(error)))

    def latest(self, path: str, predicate=None) -> dict:
        matches = [call for call in self.calls if call["path"] == path
            and (predicate is None or predicate(call))]
        if not matches:
            raise AssertionError(f"no {path} request matched; calls={self.calls[-3:]}")
        return matches[-1]


def wait_until(page: Page, check, description: str, timeout: float = 15) -> object:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        # Playwright's synchronous event handlers are dispatched while its
        # event loop runs. A plain sleep would leave evidence.calls unchanged.
        page.wait_for_timeout(150)
    raise AssertionError(f"timeout waiting for {description}")


def login(page: Page, base: str) -> None:
    page.goto(base + "/deploy")
    page.locator('input[autocomplete="username"]').fill("admin")
    page.locator('input[autocomplete="current-password"]').fill("password")
    page.get_by_role("button", name="Sign In").click()
    expect(page.get_by_role("heading", name="Deploy", exact=True)).to_be_visible()


def edit_node_labels(page: Page, evidence: Evidence, fixture: dict) -> None:
    page.goto(fixture["base_url"] + "/nodes")
    row = page.get_by_role("row").filter(has_text="browser-h100")
    expect(row).to_be_visible()
    row.get_by_role("button", name="Edit labels").click()
    dialog = page.get_by_role("dialog", name="Node labels")
    expect(dialog).to_be_visible()
    if not dialog.get_by_role("textbox", name="Label key").count():
        dialog.get_by_role("button", name="Add label").click()
    expect(dialog.get_by_role("textbox", name="Label key")).to_have_count(1)
    dialog.get_by_role("textbox", name="Label key").fill("pool")
    dialog.get_by_role("textbox", name="Label value").fill("browser")
    dialog.get_by_role("button", name="Save").click()
    expect(dialog).not_to_be_visible()
    expect(row).to_contain_text("pool=browser")
    evidence.steps.append("node label edited through Nodes page")
    page.goto(fixture["base_url"] + "/deploy")


def open_model_chooser(page: Page) -> None:
    if not page.get_by_role("dialog", name="Choose a model and running scheme").is_visible():
        page.get_by_role("button", name=re.compile(r"^Choose model$|^Change model or scheme$")).click()
    expect(page.get_by_role("dialog", name="Choose a model and running scheme")).to_be_visible()


def many_schemes(page: Page, evidence: Evidence, fixture: dict) -> None:
    response = page.request.get(fixture["base_url"] + "/api/v1/inference-templates/index")
    assert response.ok, f"template index HTTP {response.status}"
    gemma = next((item for item in response.json().get("items", [])
        if item.get("model_id") == "google/gemma-4-26b-a4b-it"), None)
    assert gemma, "Gemma 40-scheme catalog item absent from this build"
    assert len(gemma["profiles"]) == 40, len(gemma["profiles"])
    open_model_chooser(page)
    page.get_by_role("tab", name="Built-in catalog").click()
    page.get_by_role("searchbox", name="Search models").fill("gemma-4-26b-a4b-it")
    model = page.locator(".deploy-catalog-model").filter(has_text="gemma-4-26b-a4b-it")
    expect(model).to_have_count(1)
    expect(model).to_contain_text("40 schemes")
    model.click()
    section = page.locator(".deploy-catalog-schemes")
    expect(section.locator(".deploy-catalog-scheme")).to_have_count(6)
    section.get_by_role("button", name="Next").click()
    expect(section.get_by_text("Page 2 of 7")).to_be_visible()
    scheme_search = section.get_by_role("searchbox", name="Search schemes, weights, images and hardware")
    scheme_search.fill("no-such-browser-scheme")
    expect(section.get_by_text(re.compile("No schemes match"))).to_be_visible()
    scheme_search.fill("")
    evidence.steps.append("Gemma 40-scheme pagination and empty filter verified")


def choose_catalog_scheme(page: Page, evidence: Evidence, fixture: dict) -> None:
    open_model_chooser(page)
    page.get_by_role("tab", name="Built-in catalog").click()
    search = page.get_by_role("searchbox", name="Search models")
    search.fill("deployment-browser-e2e")
    model = page.locator(".deploy-catalog-model").filter(has_text="Deployment Browser E2E")
    expect(model).to_have_count(1)
    expect(model).to_contain_text("2 schemes")
    model.click()
    section = page.get_by_role("region", name="Schemes for Deployment Browser E2E")
    expect(section.locator(".deploy-catalog-scheme")).to_have_count(2)
    section.locator(".deploy-catalog-scheme").filter(has_text="sglang").click()
    wait_until(page, lambda: any(call["path"] == "evaluate" and call["status"] == 200
        and isinstance(call["request"], dict)
        and call["request"].get("template_spec_name") == "browser-sglang"
        for call in evidence.calls), "SGLang scheme evaluation")
    evaluated = evidence.latest("evaluate", lambda call: call["request"].get("template_spec_name") == "browser-sglang")
    payload = evaluated["request"]
    assert payload.get("template_id") == fixture["template_id"], payload
    assert payload.get("model_id") == fixture["model_id"], payload
    assert payload.get("engine") == "sglang", payload
    # The dictionary's defaults are the server's to state (thinning plan
    # 4.2): an untouched draft sends nothing for the switch, and the flag
    # still reaches the command through materialization.
    assert "enable_cache_report" not in payload.get("quick_params", {}), payload
    assert "--enable-cache-report" in evaluated["response"].get("command", []), evaluated["response"]
    assert payload.get("target_vendor") == "nvidia", payload
    expect(page.locator(".deploy-template-lockbar")).to_contain_text("sglang")
    badge = "Deviates from inference template" if evaluated["response"].get("template_deviated") else "Using inference template preset"
    expect(page.locator(".deploy-template-lockbar")).to_contain_text(badge)
    evidence.steps.append("catalog model separated from two schemes; SGLang selection evaluated")


def label_filter(page: Page, evidence: Evidence) -> None:
    selector = page.locator("#node_labels")
    expect(selector).to_be_visible()
    selector.fill("pool=browser")
    selector.press("Enter")
    expect(page.get_by_text(re.compile(r"Matching 1 nodes"))).to_be_visible()
    selector.fill("zone=missing")
    selector.press("Enter")
    expect(page.get_by_text(re.compile(r"No nodes match these labels"))).to_be_visible()
    page.locator(".deploy-workbench .ant-select-selection-item-remove").last.click()
    expect(page.get_by_text(re.compile(r"Matching 1 nodes"))).to_be_visible()
    wait_until(page, lambda: any(call["path"] == "evaluate" and call["status"] == 200
        and call["request"].get("node_requirements") == {"pool": ["browser"]}
        for call in evidence.calls), "matching node label evaluation")
    evidence.steps.append("label AND filter excludes empty match and restores selected node")


def custom_image_and_view_state(page: Page, evidence: Evidence, fixture: dict) -> str:
    image = page.get_by_role("combobox", name=re.compile("Image options|Selectable runtime images"))
    expect(image).to_be_visible()
    private_ref = "registry.example.test/browser/sglang:v3"
    image.fill(private_ref)
    image.press("Tab")
    expect(image).to_have_value(private_ref)
    page.locator("#name").first.fill("browser-deployment-flow")
    # The dGPU quota leg requires a node reporting a ready partition backend;
    # the fake-node acceptance environments without one (macOS) skip the leg
    # and its wire assertions, which stay identical where it runs (gpu3).
    partition_ready = bool(next(
        (cap for cap in fixture["node"].get("partition_capabilities", [])
         if cap.get("status") == "ready"), False))
    partition = page.locator(".deploy-choice-card").filter(has_text="dGPU Memory")
    if partition_ready:
        expect(partition).to_be_enabled()
        partition.click()
    eager = page.get_by_role("combobox", name="Enforce Eager Mode", exact=True)
    expect(eager).to_be_visible()
    page.locator(".deploy-feature-card").filter(has=eager).locator(".ant-select-selector").click()
    page.locator(".ant-select-dropdown:visible").get_by_title("On", exact=True).click()
    # Free arguments remain advanced; the common environment/mount editor is
    # visible without opening that section. Both share the same form draft.
    advanced_header = page.locator(".deploy-advanced-collapse .ant-collapse-header")
    if advanced_header.get_attribute("aria-expanded") != "true":
        advanced_header.click()
    page.locator("#extra_args").fill("--browser-acceptance-marker=kept")
    editor = page.locator(".deploy-workbench-row").filter(has=page.locator("#runspec-compact-env-add"))
    expect(editor.locator("#runspec-compact-env-add")).to_be_visible()
    editor.locator("#runspec-compact-env-add").click()
    editor.get_by_placeholder("KEY").last.fill("E2E_KEEP")
    editor.get_by_placeholder("VALUE").last.fill("from-browser")
    editor.locator("#runspec-compact-mount-add").click()
    editor.get_by_placeholder("Host path, for example /srv/cache").last.fill(fixture["model_path"])
    editor.get_by_placeholder("Container path, for example /cache").last.fill("/browser-extra")
    page.locator(".ant-segmented-item").filter(has_text="Expert Deploy").click()
    page.get_by_role("tab", name="Runtime Environment").click()
    # The expert renderer can use different field markup, but the form's
    # environment must survive the trip back to the quick view and the wire.
    page.locator(".ant-segmented-item").filter(has_text="Quick Deploy").click()
    expect(page.get_by_role("combobox", name=re.compile("Image options|Selectable runtime images"))).to_have_value(private_ref)
    expect(page.locator("#extra_args")).to_have_value("--browser-acceptance-marker=kept")
    assert "E2E_KEEP" in editor.locator("input").evaluate_all(
        "inputs => inputs.map(input => input.value)")
    wait_until(page, lambda: any(call["path"] == "evaluate" and call["status"] == 200
        and isinstance(call["request"], dict) and call["request"].get("image") == private_ref
        and call["request"].get("vram_limit_gib") == (16 if partition_ready else None)
        and call["request"].get("container_overrides", {}).get("env", {}).get("E2E_KEEP") == "from-browser"
        for call in evidence.calls), "custom image and environment evaluation")
    changed = evidence.latest("evaluate", lambda call: call["request"].get("image") == private_ref)
    assert changed["request"].get("quick_params", {}).get("enforce_eager") is True, changed["request"]
    assert "--disable-cuda-graph" in changed["response"].get("command", []), changed["response"]
    badge = "Deviates from inference template" if changed["response"].get("template_deviated") else "Using inference template preset"
    expect(page.locator(".deploy-template-lockbar")).to_contain_text(badge)
    evidence.steps.append("custom image, dGPU limit, extra args, and environment survived quick/expert switch")
    return private_ref


def assert_create_matches_evaluation(page: Page, evidence: Evidence, fixture: dict, private_ref: str) -> int:
    partition_ready = bool(next(
        (cap for cap in fixture["node"].get("partition_capabilities", [])
         if cap.get("status") == "ready"), False))
    before = len(evidence.calls)
    page.locator(".deploy-submit-bar").get_by_role("button", name="Deploy", exact=True).click()
    created = wait_until(page, lambda: next((call for call in evidence.calls[before:]
        if call["path"] == "deployments"), None), "create request")
    assert created["status"] == 201, created
    body = created["request"]
    evaluated = evidence.latest("evaluate", lambda call: call["status"] == 200
        and call["request"].get("image") == private_ref
        and call["request"].get("container_overrides", {}).get("env", {}).get("E2E_KEEP") == "from-browser")
    prior = evaluated["request"]
    for key in ("model_id", "template_id", "template_spec_source", "template_spec_name", "engine",
                "image", "quick_params", "extra_args", "container_overrides", "vram_limit_gib",
                "partition_backend"):
        if key in ("vram_limit_gib", "partition_backend") and not partition_ready:
            continue
        assert body.get(key) == prior.get(key), f"{key} changed evaluate→create: {prior.get(key)!r} → {body.get(key)!r}"
    assert prior.get("placement_mode") == body.get("placement_mode") == "auto", (prior, body)
    assert not prior.get("node_uuid"), prior
    prior_placement = prior.get("placement") or {}
    assert not prior_placement.get("gpus") and not prior_placement.get("gpu_indexes") and not prior_placement.get("tp"), prior
    selected = evaluated["response"]["selected_node"]
    plan = evaluated["response"]["plan"]["plan"]
    assert body["node_uuid"] == selected["uuid"], (selected, body)
    assert body["placement"]["gpus"] == plan["gpus"], (plan, body)
    assert body["placement"]["tp"] == plan["tp"], (plan, body)
    assert body.get("node_requirements") == prior.get("node_requirements"), (prior, body)
    assert "node_labels" not in body["placement"], body
    assert body["image"] == private_ref
    assert body["vram_limit_gib"] == (16 if partition_ready else None), body
    # Automatic placement preserves the quota intent; the server resolves the
    # backend from the selected node rather than the browser's previous node.
    assert not body.get("partition_backend"), body
    assert body["container_overrides"]["env"]["E2E_KEEP"] == "from-browser"
    assert any(mount.get("host_path") == fixture["model_path"] and
        mount.get("container_path") == "/browser-extra" for mount in
        body["container_overrides"].get("extra_mounts", [])), body
    deployment_id = created["response"]["id"]
    detail = page.request.get(f"{fixture['base_url']}/api/v1/deployments/{deployment_id}")
    assert detail.ok, f"deployment detail HTTP {detail.status}: {detail.text()}"
    saved = detail.json()
    assert saved["image"] == private_ref, saved
    if partition_ready:
        assert saved["partition_backend"] == "nvidia-dgpu", saved
    for key, value in body.get("quick_params", {}).items():
        # These LaunchConfig fields are bools with omitempty; false is
        # canonically absent on the wire. Tri-state fields must remain exact.
        omitted_false = {"enable_metrics", "trust_remote_code", "enable_auto_tool_choice", "lora"}
        stored = saved["quick_params"].get(key, False) if key in omitted_false else saved["quick_params"].get(key)
        assert stored == value, (key, value, saved["quick_params"])
    for flag in ("--enable-metrics", "--trust-remote-code", "--enable-auto-tool-choice", "--enable-lora"):
        command = (evaluated["response"].get("entrypoint") or []) + (evaluated["response"].get("argv") or [])
        assert flag not in command, evaluated["response"]
    assert_recorded_launch(page, fixture, deployment_id, evaluated["response"], body, evidence)
    evidence.steps.append("create request matched final evaluation and persisted image")
    return deployment_id


def normalize_port(tokens: list[str]) -> list[str]:
    normalized = list(tokens)
    for index, token in enumerate(normalized[:-1]):
        if token == "--port":
            normalized[index + 1] = "<assigned-port>"
    return normalized


def list_items(body):
    """Items of a management-API collection, strictly {"items": [...], "total": N}."""
    assert isinstance(body, dict) and set(body) == {"items", "total"}, f"want a {{items, total}} envelope, got {str(body)[:200]}"
    assert isinstance(body["items"], list) and body["total"] == len(body["items"]), f"inconsistent list envelope: {str(body)[:200]}"
    return body["items"]


def assert_recorded_launch(page: Page, fixture: dict, deployment_id: int,
                           evaluated: dict, body: dict, evidence: Evidence) -> None:
    base = fixture["base_url"]
    def find_instance() -> dict | None:
        response = page.request.get(base + "/api/v1/instances")
        assert response.ok, f"instance list HTTP {response.status}"
        return next((item for item in list_items(response.json()) if item.get("deployment_id") == deployment_id
            and item.get("entrypoint")), None)
    instance = wait_until(page, find_instance, "persisted instance launch", timeout=30)
    record_path = Path(fixture["record_path"])
    expected_name = "gpuworks-" + instance["uuid"][:12]
    def find_workload() -> dict | None:
        if not record_path.exists():
            return None
        try:
            recorded = json.loads(record_path.read_text())
        except (OSError, json.JSONDecodeError):
            return None
        return recorded if recorded.get("Name") == expected_name else None
    workload = wait_until(page, find_workload, "agent recording workload", timeout=30)
    assert instance["node_uuid"] == body["node_uuid"] == evaluated["selected_node"]["uuid"], (instance, body)
    assert instance["gpu_indexes"] == body["placement"]["gpus"] == workload["GPUIndexes"], (instance, workload)
    assert body["placement"]["tp"] == evaluated["plan"]["plan"]["tp"], (body, evaluated)
    persisted_command = (instance.get("entrypoint") or []) + (instance.get("argv") or [])
    recorded_command = workload.get("Cmd") or []
    preview_command = (evaluated.get("entrypoint") or []) + (evaluated.get("argv") or [])
    assert persisted_command == recorded_command, (persisted_command, recorded_command)
    assert normalize_port(preview_command) == normalize_port(persisted_command), (
        preview_command, persisted_command)
    assert workload.get("Entrypoint") == ["/bin/sh", "/gpuworks/pre-start.sh"], workload
    script_mount = next((mount for mount in workload.get("Mounts") or []
        if mount.get("Target") == "/gpuworks/pre-start.sh"), None)
    assert script_mount and Path(script_mount["Source"]).read_text() == (
        '#!/bin/sh\nset -eu\n' + evaluated["pre_start_script"] + '\nexec "$@"\n'), script_mount
    assert instance["image"] == workload["Image"] == evaluated["image"]
    assert instance.get("env") == evaluated.get("env"), (evaluated.get("env"), instance.get("env"))
    expected_user_mount = (fixture["model_path"], "/browser-extra", False)
    assert expected_user_mount in {(mount.get("source"), mount.get("target"), bool(mount.get("read_only")))
        for mount in evaluated.get("mounts") or []}, evaluated.get("mounts")
    actual_mounts = {(mount.get("Source"), mount.get("Target"), bool(mount.get("ReadOnly")))
        for mount in workload.get("Mounts") or []}
    assert expected_user_mount in actual_mounts, (expected_user_mount, actual_mounts)
    for key, value in (instance.get("env") or {}).items():
        assert workload.get("Env", {}).get(key) == value, (key, value, workload.get("Env"))
    for mount in instance.get("mounts") or []:
        expected = (mount.get("source"), mount.get("target"), bool(mount.get("read_only")))
        actual = {(item.get("Source"), item.get("Target"), bool(item.get("ReadOnly")))
            for item in workload.get("Mounts") or []}
        assert expected in actual, (expected, actual)
    evidence.steps.append("instance command equals agent workload; preview agrees after port assignment; image/env/mounts preserved")


def deep_link_and_manual(page: Page, evidence: Evidence, fixture: dict) -> None:
    base = fixture["base_url"]
    page.goto(f"{base}/deploy?template={fixture['template_id']}&spec_source=user&spec=browser-sglang&model={fixture['model_id']}")
    wait_until(page, lambda: any(call["path"] == "evaluate" and call["status"] == 200
        and call["request"].get("template_spec_name") == "browser-sglang"
        and call["request"].get("target_vendor") == "nvidia" for call in evidence.calls[-8:]),
        "direct SGLang route prefill")
    expect(page.locator(".deploy-template-lockbar")).to_contain_text("sglang")
    evidence.steps.append("direct SGLang route prefilled model, scheme, engine and vendor")
    edit = page.get_by_role("button", name="Edit Command Text")
    expect(edit).to_be_enabled()
    edit.click()
    page.get_by_role("button", name="Edit Command", exact=True).click()
    editor = page.locator(".deploy-command-editor")
    expect(editor).to_be_visible()
    editor.fill("echo browser-manual-command")
    page.locator(".ant-segmented-item").filter(has_text="Expert Deploy").click()
    page.get_by_role("tab", name="Runtime Environment").click()
    page.locator(".ant-segmented-item").filter(has_text="Quick Deploy").click()
    expect(page.locator(".deploy-command-editor")).to_have_value("echo browser-manual-command")
    wait_until(page, lambda: any(call["path"] == "evaluate" and call["status"] == 200
        and call["request"].get("container_overrides", {}).get("command_mode") == "manual"
        and "browser-manual-command" in str(call["request"].get("container_overrides", {}))
        for call in evidence.calls[-8:]), "manual command evaluation")
    evidence.steps.append("manual command preserved across view/runtime switch")


def mobile_no_overflow(page: Page, fixture: dict) -> None:
    page.set_viewport_size({"width": 390, "height": 844})
    page.goto(f"{fixture['base_url']}/deploy")
    open_model_chooser(page)
    page.get_by_role("tab", name="Built-in catalog").click()
    page.get_by_role("searchbox", name="Search models").fill("deployment-browser-e2e")
    page.locator(".deploy-catalog-model").first.click()
    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth + 1"), "mobile chooser overflows"
    page.locator(".deploy-catalog-scheme").filter(has_text="browser-sglang").click()
    expect(page.get_by_role("dialog", name="Choose a model and running scheme")).not_to_be_visible()
    expect(page.locator(".deploy-template-lockbar")).to_contain_text("sglang")
    expect(page.get_by_text("Configuration Evaluation", exact=True)).to_be_visible()
    expect(page.locator(".deploy-runtime-command")).to_contain_text("sglang.launch_server", timeout=30_000)
    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth + 1"), "mobile page overflows horizontally"
    assert page.locator(".deploy-feature-card").evaluate_all("cards => cards.every(card => { const r = card.getBoundingClientRect(); return r.left >= 0 && r.right <= innerWidth; })"), "mobile feature controls are clipped"


def kv_draft_round_trip(page: Page, evidence: Evidence, fixture: dict) -> None:
    page.goto(f"{fixture['base_url']}/deploy?template={fixture['template_id']}&spec_source=user&spec=browser-vllm&model={fixture['model_id']}")
    expect(page.locator(".deploy-template-lockbar")).to_contain_text("vllm")
    page.locator(".ant-segmented-item").filter(has_text="Expert Deploy").click()
    page.get_by_role("tab", name="Performance", exact=True).click()
    memory = page.get_by_role("switch", name="Tier 2 · CPU Memory", exact=True)
    disk = page.get_by_role("switch", name="Tier 3 · Shared Disk", exact=True)
    memory.focus()
    memory.press("Space")
    capacity = page.get_by_label("KV Offload to Memory", exact=True)
    expect(capacity).to_be_visible()
    expect(capacity).to_have_value("")
    capacity.fill("32")
    expect(disk).to_be_enabled()
    disk.click()
    values = {"Shared KV Directory": "/browser-kv", "Disk Read Threads": "4",
        "Disk Write Threads": "6", "Disk Tier Capacity (GiB)": "96"}
    for label, value in values.items():
        page.get_by_label(label, exact=True).fill(value)
    before = len(evidence.calls)
    memory.click()
    wait_until(page, lambda: any(call["path"] == "evaluate" and call["status"] == 200
        and "kv_offloading_size" in call["request"].get("quick_params_unset", [])
        for call in evidence.calls[before:]), "KV disabled evaluation")
    disabled = evidence.latest("evaluate", lambda call: call["status"] == 200
        and "kv_offloading_size" in call["request"].get("quick_params_unset", []))
    command = disabled["response"].get("command") or []
    assert "--kv-offloading-size" not in command and "--kv-transfer-config" not in command, command
    assert not any(mount.get("target") == "/browser-kv" for mount in disabled["response"].get("mounts", [])), disabled
    page.locator(".ant-segmented-item").filter(has_text="Quick Deploy").click()
    page.locator(".ant-segmented-item").filter(has_text="Expert Deploy").click()
    page.get_by_role("tab", name="Performance", exact=True).click()
    expect(memory).not_to_be_checked()
    expect(disk).to_be_checked()
    expect(disk).to_be_disabled()
    before = len(evidence.calls)
    memory.focus()
    memory.press("Space")
    expect(capacity).to_have_value("32")
    for label, value in values.items():
        expect(page.get_by_label(label, exact=True)).to_have_value(value)
    restored = wait_until(page, lambda: next((call for call in reversed(evidence.calls[before:])
        if call["path"] == "evaluate" and call["status"] == 200
        and call["request"].get("quick_params", {}).get("kv_offloading_size") == 32
        and call["request"].get("quick_params", {}).get("shared_kv_dir") == "/browser-kv"), None), "KV restored evaluation")
    command = restored["response"]["command"]
    # Disk tiering represents both memory and disk in the connector config;
    # the standalone memory-only flag must not also be emitted.
    assert "--kv-offloading-size" not in command, command
    connector = json.loads(command[command.index("--kv-transfer-config") + 1])
    assert connector == {"kv_connector": "OffloadingConnector", "kv_role": "kv_both",
        "kv_connector_extra_config": {"cpu_bytes_to_use": 32 * 1024 ** 3,
            "spec_name": "TieringOffloadingSpec", "secondary_tiers": [
                {"type": "fs", "root_dir": "/browser-kv", "n_read_threads": 4, "n_write_threads": 6}]}}, connector
    disk_mount = next(mount for mount in restored["response"]["mounts"] if mount["target"] == "/browser-kv")
    assert int(disk_mount["size_hint"]) == 96 * 1024 ** 3, disk_mount
    evidence.steps.append("KV 32 GiB, path, threads and disk limit survive off/view-switch/on; disabled argv and mounts exclude KV; keyboard toggles work")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--fixture", type=Path, required=True)
    parser.add_argument("--artifact-dir", type=Path, required=True)
    args = parser.parse_args()
    fixture = json.loads(args.fixture.read_text())
    args.artifact_dir.mkdir(parents=True, exist_ok=True)
    evidence = Evidence()
    deployment_id: int | None = None
    passed = False
    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(channel="chrome", headless=True)
        context = browser.new_context(locale="en-US", viewport={"width": 1440, "height": 900})
        context.add_init_script("localStorage.setItem('gpuworks.locale', 'en')")
        page = context.new_page()
        page.set_default_timeout(10_000)
        try:
            login(page, fixture["base_url"])
            evidence.watch(page)
            edit_node_labels(page, evidence, fixture)
            many_schemes(page, evidence, fixture)
            choose_catalog_scheme(page, evidence, fixture)
            label_filter(page, evidence)
            private_ref = custom_image_and_view_state(page, evidence, fixture)
            assert page.evaluate("document.documentElement.scrollWidth <= innerWidth + 1"), "desktop page overflows horizontally"
            page.screenshot(path=str(args.artifact_dir / "deployment-flow-desktop.png"), full_page=True)
            deployment_id = assert_create_matches_evaluation(page, evidence, fixture, private_ref)
            deep_link_and_manual(page, evidence, fixture)
            kv_draft_round_trip(page, evidence, fixture)
            mobile_no_overflow(page, fixture)
            assert not evidence.console_errors, evidence.console_errors
            page.screenshot(path=str(args.artifact_dir / "deployment-flow-mobile.png"), full_page=True)
            passed = True
        except BaseException:
            print(traceback.format_exc(), flush=True)
            page.screenshot(path=str(args.artifact_dir / "failure.png"), full_page=True)
            evidence.steps.append(f"failure URL: {page.url}")
            evidence.steps.append(page.locator("body").inner_text()[:4000])
            raise
        finally:
            (args.artifact_dir / "observations.json").write_text(json.dumps({
                "passed": passed, "steps": evidence.steps, "calls": evidence.calls,
                "console_errors": evidence.console_errors, "deployment_id": deployment_id,
            }, ensure_ascii=False, indent=2))
            if passed and deployment_id is not None:
                response = page.request.delete(f"{fixture['base_url']}/api/v1/deployments/{deployment_id}")
                assert response.status in (200, 202, 204), f"cleanup failed: HTTP {response.status}: {response.text()}"
                wait_until(page, lambda: page.request.get(
                    f"{fixture['base_url']}/api/v1/deployments/{deployment_id}").status == 404,
                    "deployment cleanup", timeout=30)
            context.close()
            browser.close()
    print("Chrome E2E passed: catalog selection, state preservation, evaluate/create, direct route, manual mode, mobile")


if __name__ == "__main__":
    main()
