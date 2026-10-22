#!/usr/bin/env python3
"""Real-browser acceptance for the benchmark admission dialog.

Self-contained: it starts the built server and a real agent on loopback, serves
a deliberately slow OpenAI-compatible endpoint so a server-origin run stays
active, then drives the real UI in Chrome — create a run, create a second one to
hit the admission gate, confirm the dialog, and check that the run's detail page
carries the override warning.

It is the browser half of the checks in
`artifacts/archive/2026-08-23-benchmark/plan-admission-tokenizer.md`; the managed-target
(tokenizer resolution) half needs a real deployment and stays with the unit and
RTL suites.

Requirements: `bin/gpuworks` and `bin/gpuworks-agent` (`make build`),
`pip install playwright`, and a system Chrome. Usage:

    python3 test/e2e/benchmark_admission_walkthrough.py <screenshot-dir>

Screenshots and a short README land in that directory; server and agent logs are
kept in a temporary directory named at the end of the run.
"""
import http.server
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

from playwright.sync_api import sync_playwright

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SHOTS = sys.argv[1]
os.makedirs(SHOTS, exist_ok=True)
ADDR = "127.0.0.1:8833"
BASE = f"http://{ADDR}"
ENDPOINT_PORT = 9099
ENDPOINT = f"http://127.0.0.1:{ENDPOINT_PORT}"
ADMIN = "admin"
PASSWORD = "walkthrough-password"

DATA = tempfile.mkdtemp(prefix="gpuworks-walkthrough-data-")
STATE = tempfile.mkdtemp(prefix="gpuworks-walkthrough-state-")
LOG = tempfile.mkdtemp(prefix="gpuworks-walkthrough-log-")
procs = []

# antd separates two Han characters inside a button, so "登录" renders as "登 录".
LOGIN = re.compile(r"登\s*录")
PROBE = re.compile(r"探\s*测")
START = re.compile(r"开\s*始评测")
MODE_SATURATION = "吞吐上限探测"


def log(message):
    print(f"▶ {message}", flush=True)


def start(argv, name, env=None):
    handle = open(os.path.join(LOG, f"{name}.log"), "w")
    proc = subprocess.Popen(argv, stdout=handle, stderr=subprocess.STDOUT, env={**os.environ, **(env or {})})
    procs.append((name, proc, handle))
    return proc


def stop_all():
    for name, proc, handle in procs:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
        handle.close()


def wait_for(check, what, timeout=30.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if check():
            return
        time.sleep(0.2)
    raise SystemExit(f"timed out waiting for {what}\n{tails()}")


def tails():
    out = []
    for name, _, _ in procs:
        path = os.path.join(LOG, f"{name}.log")
        if os.path.exists(path):
            with open(path) as handle:
                out.append(f"--- {name} ---\n{handle.read()[-2000:]}")
    return "\n".join(out)


class FakeEndpoint(http.server.BaseHTTPRequestHandler):
    """A deliberately slow OpenAI-compatible endpoint.

    Slow because the first benchmark run has to still be active when the second
    create is submitted: a run that finishes immediately is not a conflict.
    """

    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path.endswith("/models"):
            body = json.dumps({"data": [{"id": "fake-model"}]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_error(404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        try:
            for index in range(400):
                payload = json.dumps({"choices": [{"delta": {"content": "tok"}}], "usage": {"prompt_tokens": 8, "completion_tokens": index + 1}})
                data = f"data: {payload}\n\n".encode()
                self.wfile.write(f"{len(data):X}\r\n".encode() + data + b"\r\n")
                self.wfile.flush()
                time.sleep(0.25)
            self.wfile.write(b"0\r\n\r\n")
        except (BrokenPipeError, ConnectionResetError):
            pass


def list_items(body):
    """Items of a management-API collection, strictly {"items": [...], "total": N}."""
    assert isinstance(body, dict) and set(body) == {"items", "total"}, f"want a {{items, total}} envelope, got {str(body)[:200]}"
    assert isinstance(body["items"], list) and body["total"] == len(body["items"]), f"inconsistent list envelope: {str(body)[:200]}"
    return body["items"]


def api(path, body=None, token=None):
    request = urllib.request.Request(BASE + path, method="POST" if body is not None else "GET")
    request.add_header("Content-Type", "application/json")
    if token:
        request.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    with urllib.request.urlopen(request, data, timeout=30) as response:
        return json.loads(response.read() or "null")


def port_free(port):
    with socket.socket() as probe:
        return probe.connect_ex(("127.0.0.1", port)) != 0


def login_when_ready():
    """The health endpoint is authenticated, so readiness is the login itself."""
    try:
        return api("/api/v1/auth/login", {"username": ADMIN, "password": PASSWORD})["token"]
    except Exception:
        return None


def run_field(token, run_id, field):
    try:
        return api(f"/api/v1/benchmarks/{run_id}", token=token).get(field)
    except Exception:
        return None


def main():
    if not port_free(8833) or not port_free(ENDPOINT_PORT):
        raise SystemExit(f"ports 8833/{ENDPOINT_PORT} are already in use")

    log("starting the fake OpenAI endpoint")
    server = http.server.ThreadingHTTPServer(("127.0.0.1", ENDPOINT_PORT), FakeEndpoint)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    log("starting the server")
    # The first start creates the admin account from this file, as after an install.
    os.makedirs(DATA, exist_ok=True)
    with open(os.path.join(DATA, "initial-credentials"), "w") as f:
        json.dump({"user": "admin", "password": PASSWORD}, f)
    start([os.path.join(REPO, "bin/gpuworks"), "serve", "--addr", ADDR, "--data", DATA, "--public-url", BASE,
           "--log-level", "info"], "server", {})
    token = None
    deadline = time.time() + 30
    while token is None and time.time() < deadline:
        token = login_when_ready()
        time.sleep(0.3)
    if token is None:
        raise SystemExit("the server never accepted a login\n" + tails())
    log("server ready and admin logged in")

    node = api("/api/v1/nodes", {"name": "walkthrough-node", "server_url": BASE}, token)

    log("connecting a real agent")
    start([os.path.join(REPO, "bin/gpuworks-agent"), "run", "--server", BASE, "--token", node["token"], "--state", STATE],
          "agent", {"GPUWORKS_LOG_LEVEL": "info"})
    def node_connected():
        states = [n["state"] for n in list_items(api("/api/v1/nodes", token=token))]
        # A node's state says only whether its agent is connected; a host
        # without a GPU is online all the same.
        return any(state == "online" for state in states)

    wait_for(node_connected, "the node to come online")
    log("node states: " + json.dumps([(n["name"], n["state"]) for n in list_items(api("/api/v1/nodes", token=token))]))

    shots = []
    log("driving Chrome")
    with sync_playwright() as play:
        browser = play.chromium.launch(channel="chrome")
        # The UI follows the browser language; the walkthrough asserts on the
        # Chinese copy, so ask for it explicitly.
        context = browser.new_context(locale="zh-CN", viewport={"width": 1440, "height": 960})
        page = context.new_page()
        drawer = page.locator(".ant-drawer")

        def shot(name, note):
            page.screenshot(path=os.path.join(SHOTS, name))
            shots.append((name, note))
            print(f"  ✎ {name}: {note}", flush=True)

        page.goto(BASE, wait_until="networkidle")
        page.fill("input[autocomplete=username]", ADMIN)
        page.fill("input[type=password]", PASSWORD)
        page.get_by_role("button", name=LOGIN).click()
        page.wait_for_selector("input[type=password]", state="detached", timeout=30000)

        def open_wizard():
            page.goto(BASE + "/evaluations", wait_until="networkidle")
            # The SPA renders its shell after hydration, so wait for the button
            # itself rather than for the network to go quiet.
            page.wait_for_selector("button:has-text('新建评测')", timeout=30000)
            new_run = page.get_by_role("button", name="新建评测").first
            new_run.wait_for(state="visible", timeout=20000)
            new_run.click()
            drawer.wait_for(state="visible", timeout=15000)
            drawer.get_by_text(MODE_SATURATION).first.wait_for(state="visible", timeout=15000)
            drawer.get_by_text(MODE_SATURATION).first.click()
            # Button-style radios hide the input; the styled label is what a user
            # (and therefore a click) targets.
            external = drawer.locator("label.ant-radio-button-wrapper", has_text="外部端点")
            external.wait_for(state="visible", timeout=15000)
            external.click()
            page.wait_for_selector(".ant-drawer input#base_url", state="visible", timeout=15000)
            drawer.locator("input#base_url").fill(ENDPOINT)
            drawer.get_by_role("button", name=PROBE).click()
            drawer.locator(".ant-select-selection-item").first.wait_for(timeout=20000)
            drawer.get_by_role("button", name="下一步").click()
            page.wait_for_selector(".ant-drawer input#concurrency", state="visible", timeout=15000)

        def start_run():
            drawer.get_by_role("button", name=START).click()
            page.wait_for_url(re.compile(r".*/evaluations/\d+$"), timeout=30000)
            return page.url.rsplit("/", 1)[-1]

        open_wizard()
        drawer.get_by_text("高级设置").click()
        drawer.get_by_text("Tokenizer 来源").wait_for(state="visible", timeout=15000)
        page.wait_for_timeout(600)
        shot("wizard-tokenizer-source-external.png", "外部端点：Tokenizer 来源五选一，「自动」不可选（无目标机可解析）")
        first = start_run()
        log(f"first run #{first} created")
        wait_for(lambda: run_field(token, first, "state") in ("pending", "running"), "the first run to be active")
        time.sleep(4)

        open_wizard()
        drawer.get_by_role("button", name=START).click()
        dialog = page.locator(".ant-modal-confirm")
        dialog.wait_for(state="visible", timeout=20000)
        # Let the entrance transition finish so the screenshot shows the dialog
        # at rest rather than mid-mount.
        page.wait_for_timeout(800)
        text = dialog.inner_text()
        print("  dialog:\n" + "\n".join("    " + line for line in text.splitlines()), flush=True)
        shot("admission-conflict-dialog.png", "第二次创建命中准入：弹窗给出冲突类型与正在运行的 Run")
        assert "正在运行" in text, text
        assert "服务端" in text or "发压" in text, text

        dialog.get_by_role("button", name="仍然开始").click()
        dialog.wait_for(state="detached", timeout=20000)
        page.wait_for_url(re.compile(r".*/evaluations/\d+$"), timeout=30000)
        second = page.url.rsplit("/", 1)[-1]
        page.locator(".ant-alert-warning").first.wait_for(timeout=15000)
        detail = page.inner_text("body")
        assert "GPU 与节点资源" in detail, detail[:800]
        shot("admission-conflict-warning.png", "放行后创建成功，Run 详情页带有冲突放行警告")

        warnings = run_field(token, second, "warnings")
        print(f"  first run #{first} state={run_field(token, first, 'state')}", flush=True)
        print(f"  second run #{second} state={run_field(token, second, 'state')} warnings={warnings}", flush=True)
        assert warnings == ["BENCHMARK_CONFLICT_OVERRIDDEN"], warnings
        assert run_field(token, second, "load_origin") == "server"
        browser.close()

    server.shutdown()
    with open(os.path.join(SHOTS, "README.md"), "w") as handle:
        handle.write("# 评测准入弹窗 —— 真浏览器走查（2026-09-22）\n\n")
        handle.write("`bin/gpuworks serve` + 真实 agent（本机接入上线）+ 慢速假 OpenAI 端点，")
        handle.write("Chrome（Playwright 驱动系统 Chrome）实机点击操作。\n\n")
        handle.write("步骤：登录 → 新建评测（外部端点，发压点 server）→ 探测模型 → 开始；")
        handle.write("随后再建一个同发压点的评测，命中准入闸门并确认放行。\n\n")
        for name, note in shots:
            handle.write(f"- `{name}`：{note}\n")
        handle.write(f"\n放行后第二个 Run 的 `warnings`：`{warnings}`（即 `BENCHMARK_CONFLICT_OVERRIDDEN`）。\n")
        handle.write(f"\n服务端与 agent 日志：`{LOG}`\n")
    log("done")


if __name__ == "__main__":
    try:
        main()
    finally:
        stop_all()
        shutil.rmtree(DATA, ignore_errors=True)
        shutil.rmtree(STATE, ignore_errors=True)
        print(f"logs kept in {LOG}", flush=True)
