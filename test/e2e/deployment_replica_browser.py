"""Browser assertions for ordinary replica scale and targeted restart."""
import json
import time
from urllib.parse import urlsplit
from playwright.sync_api import expect
from local_browser_fixture import ROOT, http
from platform_token_read_revoke import button, check
import deployment_container_fixture as containers
import deployment_replica_fixture as fixture


def open_detail(page, server, name, labels):
    page.goto(server.base + "/deployments")
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    row.get_by_role("button", name=name, exact=True).click()
    drawer = page.get_by_role("dialog").last
    expect(drawer).to_be_in_viewport(ratio=1)
    expect(drawer.get_by_text(name, exact=True).first).to_be_visible()
    return drawer


def ui_ready(drawer, labels, document):
    count = document["replicas"]
    text = labels["detail"]["readyReplicas"].replace("{{ready}}", str(count)).replace("{{desired}}", str(count))
    expect(drawer.get_by_text(text, exact=True)).to_be_visible()
    expect(drawer.get_by_role("spinbutton", name=labels["detail"]["replicas"], exact=True)).to_have_value(str(count))
    expect(button(drawer, labels["replica"]["apply"])).to_be_disabled()
    ids = sorted(item["uuid"] for item in fixture.current(document))
    expect(drawer.locator('[id^="instance-"]')).to_have_count(len(document["instances"]))
    for ident in ids:
        expect(drawer.locator("#instance-" + ident)).to_be_visible()


def chat(server, key, served):
    status, value = http(server.base, "POST", "/v1/chat/completions", key, {
        "model": served, "messages": [{"role": "user", "content": "replica-recovery"}]})
    check(status == 200, f"gateway HTTP {status}: {value}")
    fixture.strict(value, {"id": "fake-engine-completion", "object": "chat.completion",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": fixture.CONTENT}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 16, "completion_tokens": 8, "total_tokens": 24}}, "gateway response")
    return value


def gone(page, ident):
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline:
        state = containers.docker_running(ident)
        if state == "absent":
            return
        page.wait_for_timeout(100)
    raise AssertionError(f"target container still present: {ident} ({state})")


# Mainline: fixture 1 real replica → unchanged Apply disabled → UI 1→2 →
# reload and independent desired/ready/container set → cancel target remove
# (no mutation) → remove original (not default newest) 2→1 → peer ID unchanged → gateway.
def scale(page, server, token, report, data, labels, drawer, requests):
    dep = data["deployment"]["id"]
    report["step"] = "scale initial exact one ready replica"
    first = fixture.ready(page, server, token, report, dep, 1)
    original = fixture.current(first)[0]
    ui_ready(drawer, labels, first)
    check(requests == [], "unchanged Apply caused mutation")
    drawer.get_by_role("spinbutton", name=labels["detail"]["replicas"], exact=True).fill("2")
    with page.expect_response(lambda r: urlsplit(r.url).path == f"/api/v1/deployments/{dep}/replicas"
                              and r.request.method == "PUT") as response:
        button(drawer, labels["replica"]["apply"]).click()
    check(response.value.status == 200, f"scale HTTP {response.value.status}")
    expected = [{"method": "PUT", "path": f"/api/v1/deployments/{dep}/replicas", "query": "", "body": {"replicas": 2}}]
    fixture.strict(requests, expected, "scale-up wire")
    report["last_step"] = "scale-up exact wire accepted"
    report["step"] = "scale-up independent desired2 ready2 runtime"
    two = fixture.ready(page, server, token, report, dep, 2)
    check(any(i["uuid"] == original["uuid"] and i["container_id"] == original["container_id"]
              for i in fixture.current(two)), "scale up replaced existing replica")
    drawer = open_detail(page, server, data["body"]["name"], labels)
    ui_ready(drawer, labels, two)
    target = original
    survivor = next(i for i in fixture.current(two) if i["uuid"] != original["uuid"])
    panel = drawer.locator("#instance-" + target["uuid"])
    button(panel, labels["replica"]["remove"]).click()
    pop = page.locator(".ant-popconfirm")
    expect(pop.get_by_text(labels["replica"]["removeConfirm"], exact=True)).to_be_visible()
    button(pop, labels["detail"]["cancel"]).click()
    expect(pop).not_to_be_visible()
    fixture.strict(requests, expected, "cancel removal wire")
    report["last_step"] = "target remove cancelled without mutation"
    unchanged = fixture.ready(page, server, token, report, dep, 2)
    fixture.strict(sorted(i["uuid"] for i in fixture.current(unchanged)),
                   sorted(i["uuid"] for i in fixture.current(two)), "cancel persistence")
    button(panel, labels["replica"]["remove"]).click()
    with page.expect_response(lambda r: urlsplit(r.url).path == f"/api/v1/deployments/{dep}/replicas"
                              and r.request.method == "PUT") as response:
        button(page.locator(".ant-popconfirm"), labels["replica"]["remove"]).click()
    check(response.value.status == 200, f"remove HTTP {response.value.status}")
    expected.append({"method": "PUT", "path": f"/api/v1/deployments/{dep}/replicas", "query": "",
                     "body": {"replicas": 1, "remove": [target["uuid"]]}})
    fixture.strict(requests, expected, "all scale wire")
    report["last_step"] = "target remove exact wire accepted"
    report["step"] = "target absent and peer survives"
    one = fixture.ready(page, server, token, report, dep, 1)
    fixture.strict([(i["uuid"], i["container_id"]) for i in fixture.current(one)],
                   [(survivor["uuid"], survivor["container_id"])], "survivor identity")
    gone(page, target["container_id"])
    one = fixture.wait(page, server, token, report, dep, lambda d: all(
        i["uuid"] != target["uuid"] for i in d["instances"]), "removed target gone from independent GET")
    drawer = open_detail(page, server, data["body"]["name"], labels)
    ui_ready(drawer, labels, one)
    report["gateway"] = chat(server, data["key"], data["body"]["served_name"])
    report["transition"] = {"initial": first, "scaled": two, "removed": one,
                            "target": target, "survivor": survivor}
    return expected


# Mainline drain: real gateway SSE first chunk → per-container log counter
# identifies exact target → UI default drain → target stays alive while its
# stream runs; failover pre-probe selects target despite inflight, post-drain
# probe must select peer with no replacement ready → full stream → replacement.
# Mainline immediate: same first-chunk barrier → UI unchecks drain → precise
# drain=false request → stream cannot complete → target removed, peer unchanged.
def restart(page, server, token, report, holder, data, labels, drawer, requests, drain):
    dep = data["deployment"]["id"]
    report["step"] = "restart initial exact two ready replicas"
    initial = fixture.ready(page, server, token, report, dep, 2)
    instances = fixture.current(initial)
    before = fixture.counters(instances)
    stream = fixture.Stream(server.base, data["key"], data["body"]["served_name"])
    holder["stream"] = stream
    deadline = time.monotonic() + 15
    while not stream.first.is_set() and not stream.done.is_set() and time.monotonic() < deadline:
        page.wait_for_timeout(100)
    check(stream.first.is_set() and not stream.done.is_set(), f"no in-flight first chunk barrier: {stream.error}")
    fixture.strict(json.loads(fixture.stream_values(stream.lines)[0]),
        {"id": "fake-engine-completion", "choices": [{"delta": {"content": fixture.CONTENT}}]}, "first delta")
    after = fixture.counters(instances)
    serving = [i for i in instances if after[i["container_id"]] - before[i["container_id"]] == 1]
    check(len(serving) == 1 and sum(after.values()) - sum(before.values()) == 1,
          f"cannot uniquely attribute stream to container: before={before}, after={after}")
    target = serving[0]
    peer = next(i for i in instances if i["uuid"] != target["uuid"])
    report["stream_barrier"] = {"before": before, "after": after, "target": target, "first": stream.lines[:2]}
    report["last_step"] = "real SSE first delta attributed to exact container"
    if drain:
        # Counterfactual: failover must still prefer this exact target while
        # its SSE is held, rather than naturally choosing an idle peer.
        probe_before = fixture.counters(instances)
        report["before_drain_gateway"] = chat(server, data["key"], data["body"]["served_name"])
        probe_after = fixture.counters(instances)
        report["before_drain_counter_probe"] = {"before": probe_before, "after": probe_after}
        fixture.assert_counter_delta(probe_before, probe_after, target["container_id"], peer["container_id"],
                                     1, 0, "failover pre-drain held-stream target+1 peer+0")
        check(not stream.done.is_set(), "pre-drain counterfactual probe missed the held stream")
    report["step"] = "target restart confirmation and wire"
    panel = drawer.locator("#instance-" + target["uuid"])
    button(panel, labels["replica"]["restart"]).click()
    modal = page.get_by_role("dialog", name=labels["replica"]["restartTitle"], exact=True)
    expect(modal).to_be_in_viewport(ratio=1)
    expect(modal.get_by_text(labels["replica"]["restartDescription"], exact=True)).to_be_visible()
    checkbox = modal.get_by_role("checkbox", name=labels["replica"]["drain"], exact=True)
    expect(checkbox).to_be_checked()
    if not drain:
        checkbox.uncheck()
        expect(checkbox).not_to_be_checked()
    check(not stream.done.is_set(), "stream ended before restart action")
    path = f"/api/v1/instances/{target['uuid']}/restart"
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "POST") as response:
        button(modal, labels["replica"]["restart"]).click()
    check(response.value.status == 202, f"restart HTTP {response.value.status}")
    expected = [{"method": "POST", "path": path,
        "query": "" if drain else "drain=false", "body": None}]
    fixture.strict(requests, expected, "target restart wire")
    report["last_step"] = "target restart exact wire accepted"
    report["step"] = "in-flight drain preservation" if drain else "in-flight immediate interruption"
    if drain:
        draining = fixture.wait(page, server, token, report, dep,
            lambda d: stream.done.is_set() or any(i["uuid"] == target["uuid"] and i["state"] == "draining" for i in d["instances"]),
            "target reaches draining", timeout=15)
        check(not stream.done.is_set() and containers.docker_running(target["container_id"]) == "true",
              "drain stopped target before in-flight stream completed")
        check(any(i["uuid"] == peer["uuid"] and i["container_id"] == peer["container_id"] for i in fixture.current(draining)),
              "drain replaced unrelated peer")
        active_before = fixture.running_containers(report)
        fixture.assert_no_surge(draining, target, peer, active_before)
        probe_before = fixture.counters(instances)
        report["during_drain_gateway"] = chat(server, data["key"], data["body"]["served_name"])
        probe_after = fixture.counters(instances)
        report["during_drain_counter_probe"] = {"before": probe_before, "after": probe_after}
        fixture.assert_counter_delta(probe_before, probe_after, target["container_id"], peer["container_id"],
                                     0, 1, "draining exclusion target+0 peer+1")
        check(not stream.done.is_set(), "drain exclusion was observed after stream completion")
        after_probe = fixture.wait(page, server, token, report, dep, lambda d: True,
                                   "independent GET after drain routing probe")
        active_after = fixture.running_containers(report)
        fixture.assert_no_surge(after_probe, target, peer, active_after)
        check(not stream.done.is_set(), "no-surge post-probe guard missed the held stream")
        report["draining_observation"] = {"before": draining, "after": after_probe,
            "running_containers_before": active_before, "running_containers_after": active_after}
    deadline = time.monotonic() + 95
    while not stream.done.is_set() and time.monotonic() < deadline:
        page.wait_for_timeout(100)
    check(stream.done.is_set(), "gateway stream did not terminate")
    report["stream"] = {"status": stream.status, "lines": list(stream.lines), "error": stream.error}
    if drain:
        check(stream.error is None, f"drained stream failed: {stream.error}")
        fixture.assert_complete(stream.lines)
    else:
        values = fixture.stream_values(stream.lines)
        check("[DONE]" not in values and len(values) < fixture.CHUNKS + 2,
              "immediate restart allowed the in-flight stream to complete")
    report["last_step"] = "in-flight stream completed" if drain else "in-flight stream interrupted"
    report["step"] = "target replacement with unchanged peer and persisted ready2"
    gone(page, target["container_id"])
    fixture.wait(page, server, token, report, dep, lambda d: all(
        i["uuid"] != target["uuid"] for i in d["instances"]), "retired target gone from independent GET")
    final = fixture.ready(page, server, token, report, dep, 2)
    check(any(i["uuid"] == peer["uuid"] and i["container_id"] == peer["container_id"] for i in fixture.current(final)),
          "targeted restart replaced unrelated replica")
    check(all(i["uuid"] != target["uuid"] and i["container_id"] != target["container_id"] for i in fixture.current(final)),
          "targeted restart did not replace target")
    drawer = open_detail(page, server, data["body"]["name"], labels)
    ui_ready(drawer, labels, final)
    report["gateway"] = chat(server, data["key"], data["body"]["served_name"])
    report["transition"] = {"initial": initial, "final": final, "target": target, "survivor": peer}
    return expected


def mutation_wire(request):
    path = urlsplit(request.url).path
    roots = ("/api/v1/deployments", "/api/v1/instances")
    if request.method not in ("PUT", "POST", "PATCH", "DELETE") or not any(
            path == root or path.startswith(root + "/") for root in roots):
        return None
    return {"method": request.method, "path": path, "query": urlsplit(request.url).query,
            "body": request.post_data_json if request.post_data else None}


def test_replicas(page, context, server, artifacts, report, locale, data, token, holder, case):
    labels = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/deployments.json").read_text())
    auth = json.loads((ROOT / f"ui/src/i18n/locales/{locale}/auth.json").read_text())
    requests, errors, bad = [], [], []
    report["requests"], report["browser_errors"], report["http_errors"] = requests, errors, bad
    authenticated = False
    page.on("pageerror", lambda e: errors.append(str(e)))
    def console(message):
        if message.type == "error":
            if not authenticated and urlsplit(message.location.get("url", "")).path == "/api/v1/auth/me" and "401" in message.text:
                return
            errors.append(message.text)
    def observed(request):
        wire = mutation_wire(request)
        if wire is not None:
            requests.append(wire)
    def response(r):
        if r.status >= 400 and not (not authenticated and urlsplit(r.url).path == "/api/v1/auth/me" and r.status == 401):
            bad.append({"method": r.request.method, "path": urlsplit(r.url).path, "status": r.status})
    page.on("request", observed)
    page.on("response", response)
    page.on("console", console)
    page.goto(server.base + "/deployments")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
        button(page, auth["submit"]).click()
    check(login.value.status == 200, "browser login")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    authenticated = True
    drawer = open_detail(page, server, data["body"]["name"], labels)
    report["last_step"] = "drawer-open"
    if case == "scale":
        expected = scale(page, server, token, report, data, labels, drawer, requests)
    else:
        expected = restart(page, server, token, report, holder, data, labels, drawer, requests, case == "drain")
    # This browser round trip also dispatches events queued during independent
    # gateway transport before the final mutation ledger is judged.
    expect(button(page.get_by_role("dialog").last, labels["replica"]["apply"])).to_be_disabled()
    fixture.strict(requests, expected, "final complete mutation ledger")
    check(not errors and not bad, f"unexpected browser errors: {errors}, {bad}")
    report["last_step"] = "replica-runtime-recovery-and-refresh"
    report["evidence"].extend([{"layer": layer} for layer in ("ui", "request", "persistence", "execution")])
