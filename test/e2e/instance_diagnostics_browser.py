"""Instance diagnostics / live stdout → stored logs, engine gauges and launch facts.

Run: make test-e2e-instance-diagnostics E2E_LOCALE=en
Synthetic GPU facts schedule a real Alpine/fake-engine container; no GPU claim.
"""
import json
import re
import shlex
import time
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect
from dialog_checks import (
    assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check
import instance_diagnostics_fixture as fixture

RANGES = {"15m": (900000, "10s"), "1h": (3600000, "30s"),
          "6h": (21600000, "2m"), "1d": (86400000, "10m"),
          "1w": (604800000, "1h"), "1mo": (2592000000, "4h"),
          "1y": (31536000000, "24h")}


def strings(locale, namespace="deployments"):
    return json.loads((ROOT / "ui/src/i18n/locales" / locale / (namespace + ".json")).read_text())


def login(page, context, server, artifacts, report, locale, username="admin"):
    auth = strings(locale, "auth")
    report["sensitive_phase"] = True
    page.goto(server.base + "/deployments")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill(username)
    page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged:
        button(page, auth["submit"]).click()
    check(logged.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        if cookie.get("value"):
            artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=strings(locale)["page"]["title"], exact=True)).to_be_visible()
    report["sensitive_phase"] = False


def open_instance(page, server, data, report):
    instance = fixture.running(page, server, data, report)
    row = page.get_by_role("row").filter(has_text=data["name"])
    expect(row).to_have_count(1)
    path = "/api/v1/instances/" + instance["uuid"] + "/logs"
    with page.expect_response(lambda r: urlsplit(r.url).path == path) as streamed:
        row.get_by_role("button", name=data["name"], exact=True).click()
    request_contract(streamed.value, path, {})
    check(streamed.value.headers.get("content-type") == "text/event-stream", "live logs are not SSE")
    report["evidence"].append({"layer": "request", "live_logs": {"method": "GET", "path": path,
        "query": {}, "body": None, "status": 200, "content_type": "text/event-stream"}})
    drawer = page.get_by_role("dialog", name=data["name"], exact=True)
    expect(drawer).to_be_in_viewport(ratio=1)
    panel = drawer.locator('[id="instance-' + instance["uuid"] + '"]')
    expect(panel).to_be_visible()
    return drawer, panel


def prove_refused_delete(page, dialog, server, dep_id, delete, report):
    """Hold Delete: loading, no second write, a 502 keeps the dialog and the deployment."""
    reason = "upstream fixture is unreachable"
    path = "/api/v1/deployments/" + str(dep_id)
    held = []

    def hold_delete(route):
        if route.request.method != "DELETE" or urlsplit(route.request.url).path != path:
            route.continue_()
            return
        held.append(route)

    page.route("**" + path, hold_delete)
    refused = None
    try:
        ok = dialog.locator(".ant-modal-footer .ant-btn-primary")
        ok.click()
        expect(ok).to_have_class(re.compile(r"\bant-btn-loading\b"))
        ok.click()
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline and not held:
            page.wait_for_timeout(100)
        check(len(held) == 1, "duplicate delete while the first request is held")
        check(urlsplit(held[0].request.url).query == "", "held delete changed the force query")
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as failed:
            held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
        refused = failed.value
    finally:
        page.unroute("**" + path, hold_delete)
    check(refused.status == 502, f"refused delete HTTP {refused.status}")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_role("checkbox", name=delete["force"], exact=True)).not_to_be_checked()
    expect(page.get_by_text(delete["failed"].replace("{{detail}}", reason), exact=True)).to_be_visible()
    stayed = page.request.get(server.base + path)
    body = stayed.json()
    check(stayed.status == 200 and body.get("id") == dep_id and body.get("desired_state") != "deleting",
          "refused delete changed the deployment")
    report["evidence"].append({"layer": "ui", "inflight_deletes": 1, "confirm_loading": True,
        "failure_status": 502, "force_checked": False, "failure_reason_visible": True, "dialog_open": True})
    report["evidence"].append({"layer": "request", "method": "DELETE", "path": path, "query": "",
        "status": 502, "body": {"message": reason}})
    report["evidence"].append({"layer": "persistence", "independent_get": path,
        "desired_state": body.get("desired_state"), "unchanged": True})
    cancel_button(delete["cancel"])(dialog)
    expect(dialog).to_be_hidden()


def prove_force_delete(page, server, name, dep_id, text, report):
    """Later force delete matches an independent 404 and the reloaded empty row."""
    delete = text["delete"]
    page.goto(server.base + "/deployments")
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=name, exact=True).click()
    drawer = page.get_by_role("dialog", name=name, exact=True)
    expect(drawer).to_be_visible()
    drawer.get_by_role("button", name=re.compile(re.escape(delete["button"]) + "$")).click()
    dialog = page.get_by_role("dialog", name=delete["title"].replace("{{name}}", name), exact=True)
    expect(dialog).to_be_visible()
    dialog.get_by_role("checkbox", name=delete["force"], exact=True).check()
    path = "/api/v1/deployments/" + str(dep_id)
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "DELETE") as deleted:
        button(dialog, delete["forceButton"]).click()
    check(deleted.value.status == 200 and urlsplit(deleted.value.url).query == "force=true",
          f"force delete HTTP {deleted.value.status} {deleted.value.url}")
    expect(page.get_by_text(delete["forced"], exact=True).first).to_be_visible()
    gone = page.request.get(server.base + path)
    check(gone.status == 404, f"force-deleted deployment GET HTTP {gone.status}")
    page.reload()
    expect(page.get_by_role("heading", name=text["page"]["title"], exact=True)).to_be_visible()
    expect(page.get_by_role("row").filter(has_text=name)).to_have_count(0)
    report["evidence"].append({"layer": "ui", "force_delete_reloaded_row": 0})
    report["evidence"].append({"layer": "request", "method": "DELETE", "path": path,
        "query": "force=true", "status": 200})
    report["evidence"].append({"layer": "persistence", "independent_get": path, "status": 404})


def exercise_delete_dialog(page, drawer, server, data, text, report):
    """Delete confirm over the detail drawer: identity, layout, focus, and dismissals."""
    name = data["name"]
    delete = text["delete"]
    title = delete["title"].replace("{{name}}", name)
    requests = []

    def seen(request):
        path = urlsplit(request.url).path
        if request.method == "DELETE" and path.startswith("/api/v1/deployments/"):
            requests.append(path)

    page.on("request", seen)
    trigger = drawer.get_by_role("button", name=re.compile(re.escape(delete["button"]) + "$"))
    expect(trigger).to_be_visible()

    def open_delete():
        trigger.click()
        dialog = page.get_by_role("dialog", name=title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = open_delete()
    assert_dialog_shell(page, dialog, dialog.get_by_text(delete["description"], exact=True),
                        button(dialog, delete["button"]))
    dialog = assert_nested_dismissals(
        dialog, drawer, open_delete,
        [("cancel", cancel_button(delete["cancel"])), ("escape", escape),
         ("close", close_button), ("mask", mask)],
        lambda: len(requests), trigger)
    check(requests == [], "delete dialog dismissal sent DELETE")
    report["evidence"].append({"layer": "ui", "delete_dialog": title,
        "dismissals": ["cancel", "escape", "close", "mask"], "delete_requests": 0})
    prove_refused_delete(page, dialog, server, data["deployment"]["id"], delete, report)
    expect(drawer).to_be_visible()


def request_contract(response, path, query, method="GET", body=None):
    request = response.request
    check(request.method == method and urlsplit(request.url).path == path, "browser request route differs")
    check(parse_qs(urlsplit(request.url).query) == {k: [str(v)] for k, v in query.items()},
          "browser query fields differ: " + request.url)
    check(request.post_data_json == body if body is not None else request.post_data is None,
          "browser request body differs")
    check(response.status == 200, f"browser {path}: HTTP {response.status}")


def select_range(page, panel, label, value):
    control = panel.get_by_role("combobox", name=label, exact=True)
    expect(control).to_have_count(1)
    select_control(page, control, value)


def select_control(page, control, value):
    # AntD non-searchable Select overlays its readonly input with the value.
    # Open using the input's real keyboard interface, then select visible text.
    control.focus()
    control.press("ArrowDown")
    expect(control).to_have_attribute("aria-expanded", "true")
    list_id = control.get_attribute("aria-controls")
    check(bool(list_id) and " " not in list_id, "range control must identify its own popup list")
    popup = page.locator(".ant-select-dropdown").filter(has=page.locator("[id=" + json.dumps(list_id) + "]"))
    expect(popup).to_have_count(1)
    option = popup.get_by_text(value, exact=True)
    expect(option).to_be_visible()
    option.click()
    expect(control).to_have_attribute("aria-expanded", "false")


def metric_query(response, instance, range_key):
    query = parse_qs(urlsplit(response.url).query)
    metric = query.get("metric", [None])[0]
    duration, step = RANGES[range_key]
    check(set(query) == {"metric", "instance", "start", "end", "step"}, "metric query fields differ")
    check(query["instance"] == [instance] and query["step"] == [step], "metric identity or step differs")
    check(int(query["end"][0]) - int(query["start"][0]) == duration, "metric window duration differs")
    request_contract(response, "/api/v1/metrics/query", {k: v[0] for k, v in query.items()})
    return metric


def gauges(page, drawer, panel, server, data, report, text):
    inst = data["instance"]
    wire = fixture.metric_wire(inst["api_port"], data["served_name"])
    # Await real collector samples via the authoritative API before exercising UI queries.
    deadline = time.monotonic() + 45
    while True:
        reply = page.request.get(server.base + "/api/v1/metrics/query", params={
            "metric": "gpuworks_infer_requests_running", "instance": inst["uuid"], "range": "15m", "step": "10s"})
        check(reply.status == 200, "known metric query failed")
        if any(s.get("points") for s in reply.json()["items"]):
            fixture.assert_series(reply.json(), inst["uuid"], wire["gpuworks_infer_requests_running"])
            break
        check(time.monotonic() < deadline, "real collector did not deliver engine gauges")
        page.wait_for_timeout(150)
    panel.get_by_role("tab", name=text["instance"]["tab"]["metrics"], exact=True).click()
    expect(panel.locator(".ant-statistic")).to_have_count(5)
    # The drawer has a separate deployment-wide metric panel above instances.
    # It legitimately polls the same instance gauges. Move its own control to
    # the annual window (30-minute cadence), so its default 15m timer cannot
    # be mistaken for an extra request from the instance control under test.
    # A replaced attempt stays on the logs tab and keeps its own range control.
    # The deployment header is the first range control in the drawer.
    panel_control = panel.get_by_role("combobox", name=text["timeRange"]["ariaLabel"], exact=True)
    expect(panel_control).to_have_count(1)
    outer = drawer.get_by_role("combobox", name=text["timeRange"]["ariaLabel"], exact=True).first
    check(outer.get_attribute("id") != panel_control.get_attribute("id"),
          "deployment and instance range controls must remain distinct")
    outer_replies = fixture.capture_gauges(page, inst["uuid"], RANGES["1y"],
        lambda: select_control(page, outer, text["timeRange"]["1y"]))
    check({metric_query(response, inst["uuid"], "1y") for response in outer_replies} == set(fixture.GAUGES),
          "outer deployment range did not complete all gauges")
    report["deployment_metric_range_fixture"] = "1y; separate deployment panel, all three new requests completed"
    checked_ranges = []
    for key in ["1h", "6h", "1d", "1w", "1mo", "1y", "15m"]:
        replies = fixture.capture_gauges(page, inst["uuid"], RANGES[key],
            lambda: select_range(page, panel, text["timeRange"]["ariaLabel"], text["timeRange"][key]))
        report.setdefault("metric_range_requests", []).append({"selection": key,
            "requests": [{"method": r.request.method, "path": urlsplit(r.url).path,
                "query": parse_qs(urlsplit(r.url).query), "status": r.status} for r in replies]})
        check(len(replies) == 3, "range change duplicated a gauge request")
        seen = []
        for response in replies:
            metric = metric_query(response, inst["uuid"], key)
            check(metric not in seen, "range change duplicated a metric")
            seen.append(metric)
            query = parse_qs(urlsplit(response.url).query)
            step = RANGES[key][1]
            step_ms = int(step[:-1]) * {"s": 1000, "m": 60000, "h": 3600000}[step[-1]]
            points = fixture.assert_series(response.json(), inst["uuid"], wire[metric],
                (int(query["start"][0]), int(query["end"][0]), step_ms))
            report.setdefault("metric_range_samples", []).append({"selection": key, "metric": metric, "points": points})
        check(set(seen) == set(fixture.GAUGES), "range change omitted a gauge")
        checked_ranges.append(key)
    # Statistic's current precision=0 formatter truncates its decimal digits.
    for title, value, unit in [("running", "2", ""), ("waitingRequests", "3", ""), ("gpuCache", "37", "%")]:
        card = panel.locator(".ant-statistic").filter(has=page.get_by_text(text["instanceMetrics"][title], exact=True))
        expect(card.locator(".ant-statistic-content-value")).to_have_text(value)
        if unit:
            expect(card.locator(".ant-statistic-content-suffix")).to_have_text(unit)
    unavailable = text["instanceMetrics"]["metricUnavailable"]
    expect(panel.get_by_text("— " + unavailable, exact=True)).to_have_count(2)
    report["evidence"].append({"layer": "request", "metric_ranges": checked_ranges,
        "gauge_wire": wire, "units": {"cache": "fraction 0.375 -> 37.5 percent -> Statistic precision=0 truncates to 37%"}})
    report["evidence"].append({"layer": "execution", "engine_metric_wire": wire,
        "absent_metrics": ["prefix cache ratio", "speculative acceptance ratio"]})
    report["evidence"].append({"layer": "persistence", "engine_metric_samples": report["metric_range_samples"]})


def launch_facts(page, panel, server, data, report, text):
    inst, container = data["instance"], data["container"]
    path = "/api/v1/instances/" + inst["uuid"] + "/launch-facts"
    deadline = time.monotonic() + 30
    while True:
        response = page.request.get(server.base + path)
        check(response.status == 200, "launch facts read failed")
        if response.json()["runtime_observation_relation"] == {"status": "current"}:
            break
        check(time.monotonic() < deadline, "runtime observation never became current")
        page.wait_for_timeout(100)
    labels = text["instance"]["launchFacts"]
    trigger = panel.get_by_role("button").filter(has=page.get_by_text(labels["title"], exact=True))
    with page.expect_response(lambda r: urlsplit(r.url).path == path) as loaded:
        trigger.click()
    request_contract(loaded.value, path, {})
    actual = fixture.inspect(container)
    image = json.loads(fixture.docker("image", "inspect", actual["Image"]))[0]
    expected, expected_image = fixture.assert_launch(loaded.value.json(), actual, image)
    facts = panel.locator(".ant-collapse-item").filter(has=page.get_by_role("button").filter(has=page.get_by_text(labels["title"], exact=True))).first
    for layer, values in [("container", expected), ("image", expected_image)]:
        facts.get_by_role("button").filter(has=page.get_by_text(labels["layer"][layer], exact=True)).click()
        block = facts.locator(".ant-collapse-item").filter(has_text=labels["layer"][layer]).last
        fields = {"container_id": "containerId", "image_id": "imageId", "entrypoint": "entrypoint",
                  "cmd": "arguments", "env": "containerConfigEnv" if layer == "container" else "imageConfigEnv",
                  "mounts": "mounts", "devices": "devices", "volumes": "volumes"}
        for field, label in fields.items():
            if field not in values:
                continue
            cell = block.locator(".ant-descriptions-row").filter(has=page.get_by_text(labels["field"][label], exact=True)).locator(".ant-descriptions-item-content")
            value = values[field]
            if (isinstance(value, dict) or isinstance(value, list) and value) and field not in {"entrypoint", "cmd"}:
                rendered = cell.inner_text()
                fixture.strict_json(json.loads(rendered), value, "rendered launch " + field)
            else:
                rendered = shlex.join(value) if isinstance(value, list) and value else value if value else "—"
                expect(cell).to_have_text(rendered)
    facts.get_by_role("button").filter(has=page.get_by_text(labels["layer"]["process"], exact=True)).click()
    expect(facts.get_by_text(labels["reason"]["not_collected"], exact=True)).to_have_count(2)
    report["evidence"].append({"layer": "execution", "launch_facts": {"container": expected,
        "image": expected_image, "process": {"status": "unknown", "reason": "not_collected"}}})
    report["evidence"].append({"layer": "persistence", "launch_observation_relation": "current"})


# Mainline: real container stdout marker → browser live SSE → known engine
# gauges / all metric quick ranges → independent Docker launch observations
# → browser Stop → persisted historical marker after reload; no real GPU claim.
def test_instance_diagnostics(page, context, server, artifacts, report, locale, data, token, holder):
    text = strings(locale)
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    login(page, context, server, artifacts, report, locale)
    drawer, panel = open_instance(page, server, data, report)
    report["step"] = "delete dialog over the detail drawer"
    exercise_delete_dialog(page, drawer, server, data, text, report)
    inst = data["instance"]
    report["step"] = "real stdout marker through browser SSE"
    lower = int(time.time() * 1000)
    line = fixture.write_marker(data["container"], "e2e-" + report["run_id"] + "-diagnostics")
    expect(panel.locator("pre")).to_contain_text(line)
    hit = fixture.persisted_marker(page, server, inst["uuid"], data["node"]["uuid"], line, lower)
    report["last_step"] = "live-and-persisted-marker"
    report["step"] = "engine metric wire and browser ranges"
    gauges(page, drawer, panel, server, data, report, text)
    report["step"] = "Docker facts and exact rendering"
    launch_facts(page, panel, server, data, report, text)
    report["step"] = "persisted log range then stop"
    panel.get_by_role("tab", name=text["instance"]["tab"]["logs"], exact=True).click()
    log_path = "/api/v1/logs/search"
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path
                              and parse_qs(urlsplit(r.url).query).get("instance") == [inst["uuid"]]) as searched:
        select_range(page, panel, text["timeRange"]["ariaLabel"], text["timeRange"]["1h"])
    query = parse_qs(urlsplit(searched.value.url).query)
    check(set(query) == {"instance", "from", "to", "limit"}, "log range query fields differ")
    check(query["instance"] == [inst["uuid"]] and query["limit"] == ["2000"]
          and int(query["to"][0]) - int(query["from"][0]) == 3600000, "log range query differs")
    request_contract(searched.value, log_path, {k: v[0] for k, v in query.items()})
    check([x for x in searched.value.json()["items"] if x["line"] == line] == [hit], "browser log range lost or changed known marker")
    expect(panel.locator("pre")).to_contain_text(line)
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/deployments/" + str(data["deployment"]["id"]) + "/stop") as stopped:
        button(drawer, text["detail"]["stop"]).click()
        popup = page.get_by_role("tooltip").filter(has_text=text["detail"]["stopConfirm"])
        button(popup, text["detail"]["stop"]).click()
    request_contract(stopped.value, "/api/v1/deployments/" + str(data["deployment"]["id"]) + "/stop", {}, "POST")
    deadline = time.monotonic() + 60
    from deployment_container_fixture import docker_running
    while docker_running(data["container"]) != "absent":
        check(time.monotonic() < deadline, "stopped diagnostic container remains")
        page.wait_for_timeout(150)
    # Stop intentionally retires the instance row, while logstore preserves its
    # history. Use the log center's real row-instance filter; never manufacture
    # a hidden instance panel or seed a resource UUID into a URL/input.
    report["step"] = "retired instance history in log center after reload"
    lower_search = int(time.time() * 1000) - 86400000
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path) as historical:
        page.goto(server.base + "/logs")
    upper_search = int(time.time() * 1000) - 86400000
    history_query = parse_qs(urlsplit(historical.value.url).query)
    check(set(history_query) == {"source", "from", "limit", "with_total"}
          and lower_search <= int(history_query["from"][0]) <= upper_search,
          "log center default 24h window differs")
    history_params = {"source": "instance", "from": history_query["from"][0], "limit": 500, "with_total": 1}
    request_contract(historical.value, log_path, history_params)
    check([x for x in historical.value.json()["items"] if x["line"] == line] == [hit],
          "stopped log center did not read the persisted marker")
    row = page.get_by_test_id("log-row").filter(has_text=line)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(line)
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path
                              and parse_qs(urlsplit(r.url).query).get("instance") == [inst["uuid"]]) as filtered:
        row.get_by_title(inst["uuid"], exact=True).click()
    request_contract(filtered.value, log_path, {**history_params, "instance": inst["uuid"]})
    check([x for x in filtered.value.json()["items"] if x["line"] == line] == [hit],
          "retired instance filter changed the persisted marker")
    expect(page.locator(".ant-select-auto-complete").get_by_role("combobox")).to_have_value(inst["uuid"])
    # Filters are intentionally unsaved; reloading resets to default 24h/all
    # instances. The exact stored row must still come from a fresh browser GET.
    lower_search = int(time.time() * 1000) - 86400000
    with page.expect_response(lambda r: urlsplit(r.url).path == log_path) as recent:
        page.reload()
    reloaded = parse_qs(urlsplit(recent.value.url).query)
    check(set(reloaded) == {"source", "from", "limit", "with_total"}
          and lower_search <= int(reloaded["from"][0]) <= int(time.time() * 1000) - 86400000,
          "reloaded log center default window differs")
    request_contract(recent.value, log_path, {**history_params, "from": reloaded["from"][0]})
    check([x for x in recent.value.json()["items"] if x["line"] == line] == [hit],
          "stopped log center reload lost or changed the stored marker")
    expect(page.get_by_test_id("log-row").filter(has_text=line)).to_have_count(1)
    report["step"] = "force delete after the diagnostic"
    prove_force_delete(page, server, data["name"], data["deployment"]["id"], text, report)
    check(errors == [], "diagnostics page JS errors: " + str(errors))
    report["evidence"].extend([
        {"layer": "ui", "stdout_marker": line, "metric_card_values": [2, 3, "37%"],
         "unknown_ratio_metrics": True, "retired_instance_history": "log center exact stored row"},
        {"layer": "request", "logs_search": {k: v[0] for k, v in query.items()}, "stop_status": 200,
         "history_filter": {**history_params, "instance": inst["uuid"]}, "history_reload": reloaded},
        {"layer": "persistence", "log_row": hit, "after_stop_reload": "same marker"},
        {"layer": "execution", "container_stopped": data["container"], "observed": "absent"}])
    report["last_step"] = "historical-logs-after-stop-and-reload"
