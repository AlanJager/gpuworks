#!/usr/bin/env python3
"""Deployment health configuration and real probe failure/recovery through Chrome.

Standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-deployment-health LOCALE=en
Requires the explicit private Docker socket and unchanged cached fake-engine.
Each run uses two ordinary containers and the real one-minute health cadence;
allow about eight minutes. GPU facts are synthetic; GPU compute is not tested.
"""
import json
import math
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check
import deployment_health_fixture as fixture
import deployment_replica_fixture as replicas
from local_business_runner import case_parser, run_case

CONTRACT = "gpuworks/deployment/health-check-configuration-runtime"


def format_ms(value):
    def trim(number):
        rounded = math.floor(number + 0.5) if number >= 10 else math.floor(number * 10 + 0.5) / 10
        return str(int(rounded)) if int(rounded) == rounded else str(rounded)
    if value >= 60000:
        return trim(value / 60000) + " min"
    if value >= 1000:
        return trim(value / 1000) + " s"
    return str(math.floor(value + 0.5)) + " ms"


def current_identity(document):
    instances = [item for item in document.get("instances", []) if item.get("replica") is not None]
    check(len(instances) == 1 and instances[0].get("state") == "running"
          and instances[0].get("container_id"), "health change lost the original single running instance")
    item = instances[0]
    return {"uuid": item["uuid"], "container_id": item["container_id"]}


def assert_health(document, entry, mode, status, after, expected_identity):
    replicas.strict(current_identity(document), expected_identity, "instance identity across health checks")
    health = document["health"]
    if health["status"] != status or not health.get("last") or health["last"]["at_ms"] <= after:
        return None
    last = health["last"]
    check(last["status"] == status and last["instance_uuid"] == expected_identity["uuid"],
          "health result belongs to a different state or instance")
    candidates = [probe for probe in fixture.probes(entry["directory"])
                  if probe["start"]["at_ms"] >= last["at_ms"]
                  and probe["start"]["at_ms"] - last["at_ms"] < 2000]
    check(candidates, "published health result has no corresponding external engine request")
    probe = candidates[0]
    if not probe["terminal"]:
        return None
    terminal = fixture.verify_probe(probe, entry["served"], mode, "cancelled" if status == "timeout" else "completed")
    if status == "timeout":
        replicas.strict(last["latency_ms"], 1000, "configured one-second probe deadline")
        elapsed = terminal["at_ms"] - probe["start"]["at_ms"]
        check(700 <= elapsed <= 1500, f"external engine cancellation did not follow the 1s deadline: {elapsed}")
    else:
        expected_http = 503 if status == "failed" else 200
        replicas.strict(last["http_status"], expected_http, "health HTTP result from real upstream")
        replicas.strict(terminal["http_status"], expected_http, "external HTTP completion")
        elapsed = terminal["at_ms"] - probe["start"]["at_ms"]
        check(abs(last["latency_ms"] - elapsed) <= 250, "published latency differs from independent engine audit")
        if mode == "slow":
            check(1900 <= elapsed < 3000 and 1900 <= last["latency_ms"] < 3000,
                  "two-second controlled engine response latency differs")
        if status == "failed":
            check("e2e-health-controlled-failure" in last.get("message", ""), "health lost the external failure reason")
    return {"health": health, "probe": probe}


# Mainline: 真server/agent/Docker→开着的健康探针→UI阈值1→真实2s上游超时取消→UI阈值3后恢复
# →外部503→恢复→UI关→guard下一轮证明无目标探针→UI开恢复；全程相同容器/PID且不自动重启。
def test_deployment_health(page, context, server, artifacts, report, locale, data, token, holder):
    text = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/common.json").read_text())["healthCheck"]
    auth = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/auth.json").read_text())
    deployments = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deployments.json").read_text())
    target, guard = data["target"], data["guard"]
    path = f"/api/v1/deployments/{target['id']}"
    requests, page_errors = [], []
    page.on("pageerror", lambda error: page_errors.append(str(error)))
    page.on("request", lambda request: requests.append({"method": request.method,
        "path": urlsplit(request.url).path, "query": urlsplit(request.url).query,
        "body": request.post_data_json}) if urlsplit(request.url).path == path + "/health-check" else None)
    report["sensitive_phase"] = True
    page.goto(server.base + "/deployments")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda response: urlsplit(response.url).path == "/api/v1/auth/login"
                              and response.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    for cookie in context.cookies():
        if cookie["value"]:
            artifacts.remember(cookie["value"])
    check(logged.value.status == 200, "health browser login failed")
    report["sensitive_phase"] = False
    expect(page.get_by_role("heading", name=deployments["page"]["title"], exact=True)).to_be_visible()
    identities, docker_identities = {}, {}
    for entry in (target, guard):
        document = fixture.wait(page, lambda: (value if value.get("ready_replicas") == 1
            and any(item.get("state") == "running" and item.get("container_id")
                    for item in value.get("instances", [])) else None)
            if (value := fixture.document(server, token, entry, report)) else None,
            entry["role"] + " real engine readiness")
        child_starts = [event for event in fixture.audit(entry["directory"]) if event["phase"] == "child_started"]
        check(len(child_starts) == 1, "engine audit wrapper did not observe exactly one child")
        entry["expected_children"] = 1
        identities[entry["role"]] = current_identity(document)
        docker_identities[entry["role"]] = fixture.inspect_identity(identities[entry["role"]]["container_id"])
        replicas.strict(docker_identities[entry["role"]]["image_id"], report["fake_engine_image_id"], "unchanged engine image")
    report["health_initial_instances"] = identities
    report["health_initial_docker"] = docker_identities

    def unchanged():
        for entry in (target, guard):
            replicas.strict(current_identity(fixture.document(server, token, entry, report)),
                            identities[entry["role"]], "same health instance UUID/container")
            replicas.strict(fixture.inspect_identity(identities[entry["role"]]["container_id"]),
                            docker_identities[entry["role"]], "same live Docker PID/start/restart identity")

    def show(status, observation=None):
        page.goto(server.base + f"/deployments?deployment={target['id']}")
        drawer = page.get_by_role("dialog", name=target["name"], exact=True)
        expect(drawer).to_be_visible()
        expect(drawer).to_be_in_viewport(ratio=1)
        label = text["status"][status]
        if status == "ok":
            label += " · " + format_ms(observation["health"]["last"]["latency_ms"])
        # Chinese deployment and health failures share the same visible label.
        # Bind the badge to the editor containing the exact health switch.
        editor = drawer.get_by_role("switch", name=text["enable"], exact=True).locator(
            'xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-space ")][1]')
        expect(editor.locator(".ant-badge-status-text")).to_have_text(label)
        return drawer

    def status_after(entry, mode, status, after):
        report["step"] = entry["role"] + "-probe-" + mode + "-" + status
        def observed():
            document = fixture.document(server, token, entry, report)
            report["health_last_observation"] = {"role": entry["role"], "health": document["health"],
                                                  "audit_tail": fixture.audit(entry["directory"])[-6:]}
            return assert_health(document, entry, mode, status, after, identities[entry["role"]])
        result = fixture.wait(page, observed, entry["role"] + " health " + status)
        report["last_step"] = report["step"]
        return result

    first = status_after(target, "ok", "ok", 0)
    status_after(guard, "slow", "ok", 0)
    drawer = show("ok", first)
    switch = drawer.get_by_role("switch", name=text["enable"], exact=True)
    threshold = drawer.get_by_role("spinbutton", name=text["threshold"], exact=True)
    expect(switch).to_be_checked()
    expect(threshold).to_have_value("5")
    expect(threshold).to_have_attribute("aria-valuemin", "1")
    expect(threshold).to_have_attribute("aria-valuemax", "600")
    number_group = drawer.locator(".ant-input-number-group-wrapper").filter(
        has=page.get_by_role("spinbutton", name=text["threshold"], exact=True))
    unit = number_group.locator(".ant-input-number-group-addon").last
    expect(unit).to_have_text({"en": "s", "zh": "秒"}[locale])
    expect(unit).to_be_visible()
    save = button(drawer, text["save"])
    expect(save).to_be_disabled()
    threshold.press("ArrowUp")
    expect(threshold).to_have_value("6")
    expect(save).to_be_enabled()
    threshold.press("ArrowDown")
    expect(threshold).to_have_value("5")
    expect(save).to_be_disabled()
    replicas.strict(requests, [], "numeric keyboard step must not save implicitly")
    threshold.fill("")
    expect(save).to_be_disabled()
    replicas.strict(requests, [], "empty threshold must not mutate")
    threshold.fill("5")
    expect(save).to_be_disabled()

    expected_requests = []
    def update(enabled, seconds):
        nonlocal drawer
        report["step"] = f"health-config-{enabled}-{seconds}"
        if enabled and drawer.get_by_role("switch", name=text["enable"], exact=True).is_checked():
            drawer.get_by_role("spinbutton", name=text["threshold"], exact=True).fill(str(seconds))
            trigger = button(drawer, text["save"])
        else:
            trigger = drawer.get_by_role("switch", name=text["enable"], exact=True)
        expected = {"method": "PUT", "path": path + "/health-check", "query": "",
                    "body": {"enabled": enabled, "threshold_sec": seconds}}
        with page.expect_response(lambda response: urlsplit(response.url).path == path + "/health-check"
                                  and response.request.method == "PUT") as response:
            trigger.click()
        check(response.value.status == 200, f"health setting PUT HTTP {response.value.status}")
        expected_requests.append(expected)
        replicas.strict(requests, expected_requests, "actual browser health request count/order/body")
        independent = fixture.document(server, token, target, report)
        replicas.strict(independent["health"]["threshold_sec"], seconds, "persisted health threshold")
        check((independent["health"]["status"] != "off") == enabled, "persisted health enabled differs")
        report["evidence"].append({"layer": "request", **expected, "status": response.value.status})
        report["evidence"].append({"layer": "persistence", "enabled": enabled, "threshold_sec": seconds,
                                   "independent_get": path})
        unchanged()
        return independent

    update(True, 1)
    fixture.write_control(target["directory"], "slow")
    timeout = status_after(target, "slow", "timeout", first["health"]["last"]["at_ms"])
    drawer = show("timeout")
    unchanged()
    update(True, 3)
    recovered = status_after(target, "slow", "ok", timeout["health"]["last"]["at_ms"])
    drawer = show("ok", recovered)
    fixture.write_control(target["directory"], "fail")
    failed = status_after(target, "fail", "failed", recovered["health"]["last"]["at_ms"])
    drawer = show("failed")
    unchanged()
    fixture.write_control(target["directory"], "ok")
    healthy = status_after(target, "ok", "ok", failed["health"]["last"]["at_ms"])
    drawer = show("ok", healthy)
    # A completed independent guard round closes the preceding round before
    # disabling. The next guard's two-second response is a bounded round witness.
    guard_before = status_after(guard, "slow", "ok", healthy["health"]["last"]["at_ms"] - 2000)
    before_count = len(fixture.probes(target["directory"]))
    update(False, 3)
    drawer = show("off")
    expect(drawer.get_by_role("switch", name=text["enable"], exact=True)).not_to_be_checked()
    expect(drawer.get_by_role("spinbutton", name=text["threshold"], exact=True)).to_have_count(0)
    guard_after = status_after(guard, "slow", "ok", guard_before["health"]["last"]["at_ms"])
    replicas.strict(len(fixture.probes(target["directory"])), before_count,
                    "disabled target emitted a probe during the next independently witnessed health round")
    unchanged()
    report["health_off_round_witness"] = {"before": guard_before, "after": guard_after,
        "target_probe_count_before": before_count, "target_probe_count_after": before_count}
    update(True, 3)
    final = status_after(target, "ok", "ok", healthy["health"]["last"]["at_ms"])
    drawer = show("ok", final)
    expect(drawer.get_by_role("switch", name=text["enable"], exact=True)).to_be_checked()
    expect(drawer.get_by_role("spinbutton", name=text["threshold"], exact=True)).to_have_value("3")
    expect(button(drawer, text["save"])).to_be_disabled()
    unchanged()
    replicas.strict(requests, expected_requests, "final exactly four UI health mutations")
    check(not page_errors, f"health page errors: {page_errors}")
    report["evidence"].extend([
        {"layer": "ui", "enabled_states": [True, False, True], "thresholds": [5, 1, 3],
         "states": ["ok", "timeout", "ok", "failed", "ok", "off", "ok"], "locale": locale},
        {"layer": "execution", "target_audit": fixture.audit(target["directory"]),
         "guard_audit": fixture.audit(guard["directory"]), "timeout": timeout,
         "slow_recovery": recovered, "failed": failed, "final": final},
        {"layer": "runtime", "unchanged_docker": docker_identities,
         "semantics": "next health round applies configuration; no container replacement or restart"}])
    report["last_step"] = "health-restored-and-instance-identities-unchanged"


def main():
    parser = case_parser(__doc__)
    args = parser.parse_args()
    return run_case(args, contract_id=CONTRACT,
        manifest="test/e2e/coverage/deployment-health.json", test_file="test/e2e/deployment_health.py",
        prepare=fixture.prepare, exercise=test_deployment_health, cleanup=fixture.cleanup,
        helpers=("test/e2e/deployment_health_fixture.py", "test/e2e/deployment_replica_fixture.py",
                 "test/e2e/deployment_container_fixture.py", "test/e2e/deployment_lifecycle.py",
                 "test/e2e/platform_token_read_revoke.py", "test/e2e/testdata/deployment-health-proxy/main.go"))


if __name__ == "__main__":
    raise SystemExit(main())
