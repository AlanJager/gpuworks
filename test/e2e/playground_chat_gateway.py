#!/usr/bin/env python3
"""Playground chat: real streaming through the gateway and conversation isolation.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-playground-chat-gateway
After build: python3 test/e2e/playground_chat_gateway.py --locale en
Requires Python Playwright, system Chrome, Docker and the built server/agent.
The GPU facts are injected only so the scheduler places the deployment; the
container, engine and gateway request are real. No real GPU compute is claimed.
Artifacts: secrets/e2e/<runId>; the runner stops its own server/agent and
retains the private database/logs for scoped diagnosis (seven days).
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import traceback
from urllib.parse import quote, urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import button, check
from playground_gateway_fixture import DEPLOYMENTS, MODELS, NODES, cleanup as cleanup_resources

CONTRACT = "gpuworks/playground/chat-streaming-conversations"
CASE = "Playground / 真实流式对话 / 部署入口、网关 SSE、刷新持久化与新对话隔离"
MODEL_DIR = ROOT / "test/e2e/testdata/fake-model"
IMAGE = "gpuworks/fake-engine:test"
CHAT = "/api/v1/playground/v1/chat/completions"
MARKER = "hello from fake-engine"


def strings(locale):
    deployments = json.loads((ROOT / "ui/src/i18n/locales" / locale / "deployments.json").read_text())
    playground = json.loads((ROOT / "ui/src/i18n/locales" / locale / "playground.json").read_text())
    auth = json.loads((ROOT / "ui/src/i18n/locales" / locale / "auth.json").read_text())
    return deployments, playground, auth


def wait_online(base, token, node_uuid, agent):
    deadline = time.time() + 40
    while time.time() < deadline:
        status, node = http(base, "GET", NODES + f"/{node_uuid}", token=token)
        if status == 200 and node.get("state") == "online":
            return node
        if agent.poll() is not None:
            raise AssertionError("the agent exited before the node came online")
        time.sleep(0.5)
    raise AssertionError("the node never came online")


def wait_api(page, server, dep_id, predicate, timeout=90, label="deployment"):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        response = page.request.get(server.base + DEPLOYMENTS + f"/{dep_id}")
        if response.status == 404:
            last = "404"
        elif response.status == 200:
            last = response.json()
            if predicate(last):
                return last
        time.sleep(0.5)
    raise AssertionError(f"{label} stayed {last}")


INJECT = {"docker_error": False, "cleanup_timeout": False}


def docker_running(container_id):
    if not container_id:
        return "absent"
    if INJECT["docker_error"]:
        raise AssertionError("injected docker daemon error")
    result = subprocess.run(
        ["docker", "inspect", "--format", "{{.State.Running}}", container_id],
        capture_output=True, text=True)
    if result.returncode == 0:
        state = result.stdout.strip()
        check(state in ("true", "false"), f"docker inspect returned an invalid state: {state!r}")
        return state
    missing = r"Error(?: response from daemon)?: No such (?:container|object): " + re.escape(container_id)
    if re.fullmatch(missing, result.stderr.strip(), flags=re.IGNORECASE):
        return "absent"
    raise AssertionError(
        f"docker inspect failed for {container_id}: {result.stderr.strip()}")


def check_new_chat_request(body, served_name, prompt):
    check(body.get("model") == served_name and body.get("stream") is True,
          "a fresh conversation used the wrong model or streaming mode")
    check(body.get("messages") == [{"role": "user", "content": prompt}],
          "a fresh conversation must send exactly its own user prompt")


def stream_shape(text):
    """Parse a real SSE body into the parts 20.3 has to assert separately.

    Counting the marker in the whole body cannot tell a complete stream from
    one that stopped after its first chunk, so the events are split out here:
    content deltas, the terminal event, error events and the usage event.
    """
    events, data = [], []
    for line in text.splitlines():
        if not line.strip():
            if data:
                events.append("\n".join(data)); data = []
            continue
        if line.startswith("data:"):
            data.append(line[len("data:"):].strip())
    if data:
        events.append("\n".join(data))
    shape = {"events": len(events), "content_deltas": 0, "done_last": False,
             "usage": None, "error_events": 0, "terminal": None}
    for index, event in enumerate(events):
        if event == "[DONE]":
            shape["done_last"] = index == len(events) - 1
            shape["terminal"] = "[DONE]"
            continue
        if event.startswith("[DONE]"):
            shape["done_last"] = index == len(events) - 1
            shape["terminal"] = "[DONE]"
            continue
        try:
            payload = json.loads(event)
        except json.JSONDecodeError:
            continue
        if payload.get("error"):
            shape["error_events"] += 1
            continue
        usage = payload.get("usage")
        if isinstance(usage, dict):
            shape["usage"] = usage
        for choice in payload.get("choices") or []:
            if (choice.get("delta") or {}).get("content"):
                shape["content_deltas"] += 1
    return shape


def metric_value(page, label):
    """Read one reply metric value by its label.

    Reply metrics render as <small>label</small><strong>value</strong> inside
    the message, so the label is matched exactly and the value read from the
    sibling element rather than from the whole message text.
    """
    metric = page.locator(".playground-metrics").first.locator(
        "span").filter(has=page.locator("small", has_text=re.compile("^" + re.escape(label) + "$")))
    if not metric.count():
        return None
    return metric.first.locator("strong").inner_text().strip()


def send_and_capture(page, composer, prompt, timeout=60000):
    """Send one Playground message and return its streaming request/response."""
    composer.fill(prompt)
    send = page.locator(".playground-composer-actions button.ant-btn-primary")
    expect(send).to_be_enabled()
    with page.expect_response(lambda r: urlsplit(r.url).path == CHAT
                              and r.request.method == "POST", timeout=timeout) as reply:
        send.click()
    response = reply.value
    body = response.request.post_data_json
    text = response.text()
    return response, body, text


# Contract: gpuworks/playground/chat-streaming-conversations
# Mainline: 独立 server/SQLite + 真实 agent/Docker + fake-engine，模型配置 openai.chat
#           → 从部署列表进入 Playground → 真实 SSE 请求经网关到引擎并渲染回复
#           → 刷新后对话仍在（IndexedDB）→ 新对话隔离，切换回旧对话内容正确
#           → 删除部署、停止 agent、清理节点与模型
def vector_operation(page, server, report, playground, served_name):
    """20.1/20.2 for the non-chat operations this deployment actually declares.

    The Playground must dispatch each declared operation to its own renderer
    and send that operation's method, path and body, not reuse the chat wire.
    """
    text = playground["vector"]
    rows = []
    # Enter through the product's own operation links instead of a hand-built
    # URL: opening an operation the model does not declare lists every
    # operation it does declare, and those links are built by the same route
    # function the deployment entry uses.
    undeclared = (server.base + "/playground/audio?model=" + quote(served_name, safe=""))
    for kind, operation, path, detail, mode in [
            ("embedding", "openai.embeddings", "/api/v1/playground/v1/embeddings", "embedding", "vector"),
            ("rerank", "openai.rerank", "/api/v1/playground/v1/rerank", "ranked", "vector")]:
        page.goto(undeclared)
        expect(page.get_by_text(
            playground["page"]["unsupportedModel"].replace("{{model}}", served_name),
            exact=True)).to_be_visible()
        page.get_by_role("link", name=operation, exact=True).click()
        expect(page).to_have_url(re.compile(r"/playground/" + mode + r"\?" + r".*operation="
                                            + re.escape(operation)))
        if kind == "rerank":
            page.get_by_text(text["rerank"], exact=True).first.click()
        input_box = page.get_by_role("textbox").first
        prompt = "GPU inference runs on accelerators " + kind
        input_box.fill(prompt)
        if kind == "rerank":
            documents = page.get_by_role("textbox").nth(1)
            documents.fill("GPU inference runs on accelerators.\nUnrelated text.")
        with page.expect_response(lambda r: urlsplit(r.url).path == path
                                  and r.request.method == "POST") as sent:
            button(page, text["run"]).click()
        response = sent.value
        check(response.status == 200, f"{operation} HTTP {response.status}")
        body = response.request.post_data_json
        check(body.get("model") == served_name, f"{operation} wrong model: {body.get('model')}")
        if kind == "embedding":
            check(body.get("input") == prompt, f"{operation} lost the input: {body}")
            check("messages" not in body and "stream" not in body,
                  f"{operation} reused the chat wire: {body}")
            payload = response.json()
            vectors = [entry.get("embedding") for entry in payload.get("data") or []]
            check(len(vectors) == 1 and isinstance(vectors[0], list) and len(vectors[0]) >= 2
                  and all(isinstance(value, float) for value in vectors[0]),
                  f"{operation} returned no numeric embedding: {payload}")
        else:
            check(body.get("query") == prompt, f"{operation} lost the query: {body}")
            check(len(body.get("documents") or []) == 2, f"{operation} lost documents: {body}")
            payload = response.json()
            results = payload.get("results") or []
            check([entry.get("index") for entry in results] == [0, 1]
                  and all(isinstance(entry.get("relevance_score"), (int, float))
                          and not isinstance(entry.get("relevance_score"), bool)
                          for entry in results),
                  f"{operation} returned no ranked results: {payload}")
            scores = [float(entry["relevance_score"]) for entry in results]
            check(scores == sorted(scores, reverse=True),
                  f"{operation} results are not ordered by score: {scores}")
        rows.append({"operation": operation, "renderer": kind, "method": "POST",
                     "path": path, "status": response.status, "model": body.get("model"),
                     "detail": detail})
    report["evidence"].append({"layer": "request", "operation_dispatch": rows,
                               "source": "each declared non-chat operation opened its own renderer and "
                                         "sent its own method/path/body; no chat wire reuse"})
    return rows


def test_playground_chat(page, context, server, artifacts, report, locale,
                         deployment, run_id):
    deployments, playground, auth = strings(locale)
    dep_id, dep_name, served_name = deployment["id"], deployment["name"], deployment["served_name"]
    first_prompt = f"first playground prompt {run_id[-8:]}"
    second_prompt = f"second playground prompt {run_id[-8:]}"
    page_errors, console_errors, bad_responses = [], [], []
    authenticated = False
    page.on("pageerror", lambda error: page_errors.append(str(error)))

    def console_seen(message):
        if message.type == "error":
            path = urlsplit(message.location.get("url", "")).path
            if (not authenticated and path == "/api/v1/auth/me"
                    and "401" in message.text):
                return
            console_errors.append({"text": message.text, "path": path})

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if not authenticated and path == "/api/v1/auth/me" and response.status == 401:
                return
            bad_responses.append({"method": response.request.method, "path": path,
                                  "status": response.status})

    page.on("console", console_seen)
    page.on("response", response_seen)

    page.goto(server.base + "/deployments")
    page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
    page.get_by_label(auth["password"], exact=True).fill("password")
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                              and r.request.method == "POST") as logged_in:
        button(page, auth["submit"]).click()
    check(logged_in.value.status == 200, "browser login failed")
    for cookie in context.cookies():
        artifacts.remember(cookie["value"])
    expect(page.get_by_role("heading", name=deployments["page"]["title"],
                            exact=True)).to_be_visible()
    authenticated = True
    report["preflight"] = "PASS"

    running = wait_api(page, server, dep_id,
                       lambda d: d.get("runtime_state") == "running"
                       and any(i.get("state") == "running" and i.get("container_id") for i in
                               d.get("instances") or [])
                       and any(op.get("type") == "openai.chat" for op in d.get("operations") or []),
                       label="running deployment with chat")
    instance = next(iter(running.get("instances") or []))
    container = instance["container_id"]
    report["container_id"] = container
    report["containers"] = [container]
    check(docker_running(container) == "true", "the deployment container is not running")
    operations = running.get("operations") or []
    check(any(op.get("type") == "openai.chat" and op.get("endpoint") == "/v1/chat/completions"
              for op in operations), f"chat operation missing: {operations}")

    row = page.get_by_role("row").filter(has_text=dep_name)
    expect(row).to_have_count(1)
    with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/playground/models"
                              and r.request.method == "GET") as catalog:
        row.get_by_role("button", name=deployments["page"]["openPlayground"]).click()
    check(catalog.value.status == 200, f"playground catalog HTTP {catalog.value.status}")
    catalog_models = catalog.value.json().get("models") or []
    model_ids = [model.get("id") for model in catalog_models]
    check(served_name in model_ids, f"playground does not list {served_name}: {model_ids}")
    expect(page).to_have_url(re.compile(r"/playground/chat\?"
                                        + r".*operation=openai\.chat"))
    check(f"model={served_name}" in page.url, f"entry lost the model: {page.url}")
    expect(page.get_by_text(served_name, exact=True).first).to_be_visible()
    report["last_step"] = "playground-open-from-deployment"

    first_response, first_body, first_text = send_and_capture(
        page, page.get_by_label(playground["chat"]["inputAria"]), first_prompt)
    check(first_response.status == 200, f"chat HTTP {first_response.status}")
    check_new_chat_request(first_body, served_name, first_prompt)
    check(first_body.get("model") == served_name, f"wrong model: {first_body.get('model')}")
    check(first_body.get("stream") is True, f"the Playground did not stream: {first_body}")
    check(any(message.get("content") == first_prompt
              for message in first_body.get("messages") or []),
          f"prompt not sent: {first_body.get('messages')}")
    check(MARKER in first_text and "[DONE]" in first_text,
          f"unexpected SSE body: {first_text[:300]}")
    first_stream = stream_shape(first_text)
    check(first_stream["done_last"],
          f"the stream did not end with a [DONE] event: {first_stream}")
    check(first_stream["content_deltas"] >= 1,
          f"no content delta arrived: {first_stream}")
    check(first_stream["usage"] is not None,
          f"the stream carried no usage event: {first_stream}")
    check(first_stream["usage"].get("total_tokens", 0) > 0,
          f"the usage event reported no tokens: {first_stream['usage']}")
    check(first_stream["error_events"] == 0,
          f"the successful stream reported error events: {first_stream}")
    # The reply must actually use the usage event, not just receive it: with
    # usage the token figure is the reported exact number, and the product
    # marks it with an approximation sign only when no usage arrived.
    reported = first_stream["usage"]["completion_tokens"]
    tokens = metric_value(page, "Token")
    check(tokens is not None, "the reply shows no token metric")
    check(not tokens.startswith("≈"),
          f"the reply treated the reported usage as an estimate: {tokens}")
    check(tokens.replace(",", "") == str(reported),
          f"the reply does not show the reported {reported} tokens: {tokens}")
    # The prompt is also the conversation title in the sidebar; message
    # assertions belong to the chat log.
    chat_log = page.locator(".playground-message-list").first
    expect(chat_log.get_by_text(first_prompt, exact=True)).to_be_visible()
    expect(chat_log.get_by_text(MARKER, exact=True).first).to_be_visible(timeout=30000)
    expect(page.locator(".playground-stop-button")).to_have_count(0, timeout=30000)
    report["last_step"] = "playground-first-reply"

    page.reload()
    expect(page.get_by_label(playground["chat"]["inputAria"])).to_be_visible()
    expand = page.get_by_role("button", name=playground["conversations"]["expand"])
    if expand.count():
        expand.click()
    conversation = page.locator(".playground-conversation-open").filter(
        has_text=first_prompt)
    expect(conversation).to_have_count(1)
    conversation.click()
    expect(chat_log.get_by_text(first_prompt, exact=True)).to_be_visible()
    expect(chat_log.get_by_text(MARKER, exact=True).first).to_be_visible()
    report["last_step"] = "playground-reload-persisted"

    new = page.get_by_role("button", name=re.compile(
        re.escape(playground["conversations"]["new"]) + "$"))
    new.click()
    expect(chat_log.get_by_text(first_prompt, exact=True)).to_have_count(0)
    second_response, second_body, second_text = send_and_capture(
        page, page.get_by_label(playground["chat"]["inputAria"]), second_prompt)
    check(second_response.status == 200, f"second chat HTTP {second_response.status}")
    check(second_body.get("model") == served_name, "second chat used another model")
    check(second_body.get("stream") is True, "the second chat was not streamed")
    check_new_chat_request(second_body, served_name, second_prompt)
    check(MARKER in second_text and "[DONE]" in second_text,
          "second SSE body is incomplete")
    expect(chat_log.get_by_text(second_prompt, exact=True)).to_be_visible()
    expect(page.locator(".playground-conversation-open")).to_have_count(2)
    expect(chat_log.get_by_text(first_prompt, exact=True)).to_have_count(0)
    first_conversation = page.locator(".playground-conversation-open").filter(
        has_text=first_prompt)
    first_conversation.click()
    expect(chat_log.get_by_text(first_prompt, exact=True)).to_be_visible()
    expect(chat_log.get_by_text(second_prompt, exact=True)).to_have_count(0)
    report["last_step"] = "playground-conversations-isolated"

    # The conversation is per account and lives in the browser: a second
    # context shows no conversations for the same server.
    other = context.browser.new_context(
        viewport=report["viewport"],
        locale="zh-CN" if locale == "zh" else "en-US", timezone_id="UTC")
    try:
        page2 = other.new_page()
        page2.goto(server.base + "/deployments")
        page2.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
        page2.get_by_label(auth["password"], exact=True).fill("password")
        with page2.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login"
                                   and r.request.method == "POST") as other_login:
            button(page2, auth["submit"]).click()
        check(other_login.value.status == 200, "second context login failed")
        page2.goto(server.base + "/playground/chat?operation=openai.chat&model=" + served_name)
        other_expand = page2.get_by_role("button", name=playground["conversations"]["expand"])
        if other_expand.count():
            other_expand.click()
        expect(page2.locator(".playground-conversation-open")).to_have_count(0)
    finally:
        other.close()
    report["last_step"] = "playground-conversations-browser-scoped"

    unexpected = [item for item in bad_responses]
    check(not unexpected, "unexpected browser HTTP errors: " + str(unexpected))
    check(not page_errors and not console_errors,
          f"browser errors: {page_errors}, {console_errors}")
    report["last_step"] = "browser-errors-clear"

    vector_operation(page, server, report, playground, served_name)
    report["last_step"] = "playground-operation-dispatch"

    report["evidence"].extend([
        {"layer": "ui", "entry": deployments["page"]["openPlayground"], "row": dep_name,
         "url": page.url, "model": served_name, "send": playground["chat"]["send"],
         "first_prompt": first_prompt, "second_prompt": second_prompt,
         "marker": MARKER, "conversations": 2},
        {"layer": "request",
         "catalog": {"path": "/api/v1/playground/models", "status": catalog.value.status},
         "first": {"path": CHAT, "status": first_response.status,
                   "model": first_body.get("model"), "stream": first_body.get("stream"),
                   "messages": first_body.get("messages")},
         "second": {"path": CHAT, "status": second_response.status,
                    "model": second_body.get("model"), "stream": second_body.get("stream"),
                    "messages": second_body.get("messages")}},
        {"layer": "persistence",
         "after_reload": True, "conversation_titles": [first_prompt, second_prompt],
         "new_conversation_empty": True, "other_browser_context_empty": True},
        {"layer": "execution",
         "sse_marker": MARKER, "sse_done": "[DONE]" in first_text,
         "container": {"id": container, "running": "true"},
         "operations": operations},
    ])
    report["browser_errors"] = {"page": page_errors, "console": console_errors,
                                "http": bad_responses}
    report["chat_sse_sample"] = artifacts.redact(first_text[:2000])


def create_fixtures(server, token, run_id, report, holder, agent_bin, artifacts,
                    inject_online_failure=False, inject_model_failure=False,
                    inject_config_failure=False, inject_deployment_failure=False):
    status, created = http(server.base, "POST", NODES, token=token,
                           body={"name": f"e2e-{run_id}-playground-node",
                                 "server_url": server.base})
    check(status == 201, f"node fixture HTTP {status}")
    node, node_token = created["node"], created["token"]
    report["owned_node_uuid"] = node["uuid"]
    artifacts.remember(node_token)
    state = server.work / "agent-state"
    state.mkdir()
    log = (server.work / "agent.log").open("w")
    holder["log"] = log
    env = {**os.environ, "GPUWORKS_FAKE_GPU": "A100x1,24576",
           "GPUWORKS_FAKE_GPU_NO_INJECT": "1"}
    agent = subprocess.Popen([str(agent_bin), "run",
                              "--server", server.base, "--token", node_token,
                              "--state", str(state)],
                             stdout=log, stderr=subprocess.STDOUT, text=True, env=env)
    holder["agent"], holder["log"] = agent, log
    if inject_online_failure:
        raise RuntimeError("injected online wait failure")
    wait_online(server.base, token, node["uuid"], agent)
    if inject_model_failure:
        raise RuntimeError("injected model registration failure")
    status, model = http(server.base, "POST", MODELS + "/local", token=token, body={
        "node_uuid": node["uuid"], "path": str(MODEL_DIR),
        "source_id": f"e2e/{run_id}-playground", "name": f"e2e-{run_id}-model"})
    check(status in (200, 201), f"model fixture HTTP {status}")
    report["owned_model_id"] = model["id"]
    status, configured = http(server.base, "PATCH",
                              MODELS + f"/{model['id']}/inference-config", token=token,
                              body={"operations": [{"type": "openai.chat"},
                                                   {"type": "openai.embeddings", "endpoint": "/v1/embeddings"},
                                                   {"type": "openai.rerank", "endpoint": "/v1/rerank"}],
                                    "input_modalities": ["text"]})
    check(status == 200, f"inference config HTTP {status} {configured}")
    if inject_config_failure:
        raise RuntimeError("injected inference config failure")
    dep_name = f"e2e-{run_id}-playground"
    served_name = f"e2e-{run_id}-served"
    status, deployment = http(server.base, "POST", DEPLOYMENTS, token=token, body={
        "model_id": model["id"], "node_uuid": node["uuid"], "name": dep_name,
        "served_name": served_name, "engine": "vllm", "image": IMAGE,
        "placement_mode": "manual",
        "container_overrides": {"command_mode": "engine"}})
    check(status in (200, 201), f"deployment fixture HTTP {status} {deployment}")
    deployment["name"], deployment["served_name"] = dep_name, served_name
    report["owned_deployment_id"] = deployment["id"]
    if inject_deployment_failure:
        raise RuntimeError("injected deployment fixture failure")
    return node, node_token, model, deployment


def cleanup(server, token, report, holder):
    return cleanup_resources(server, token, report, holder,
                             http=http, docker_running=docker_running, inject=INJECT)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--agent-bin", type=Path, default=ROOT / "bin/gpuworks-agent")
    parser.add_argument("--artifact-dir", type=Path)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--inject-online-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-model-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-config-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-deployment-failure", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-docker-error", action="store_true",
                        help=argparse.SUPPRESS)
    parser.add_argument("--inject-cleanup-timeout", action="store_true",
                        help=argparse.SUPPRESS)
    args = parser.parse_args()
    INJECT["docker_error"] = args.inject_docker_error
    INJECT["cleanup_timeout"] = args.inject_cleanup_timeout
    os.umask(0o077)
    check(shutil.which("docker") is not None, "docker is required for this lane")
    check(subprocess.run(["docker", "image", "inspect", IMAGE],
                         capture_output=True).returncode == 0,
          f"{IMAGE} is missing; run make fake-engine-image")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    manifest_path = ROOT / "test/e2e/coverage/playground-chat-gateway.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["contract_id"] == CONTRACT
          and manifest["test_function"] == test_playground_chat.__name__,
          "coverage manifest does not bind this case")
    report = {"contract": CONTRACT, "case": CASE, "run_id": run_id, "status": "NOT_RUN",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-agent-docker-fake-gpu-facts", "identity": "admin browser / node agent / Docker",
        "locale": args.locale, "timezone": "UTC", "viewport": {"width": 1440, "height": 900},
        "selection": [CONTRACT], "retries": 0, "evidence": [],
        "coverage_manifest_sha256": digest(manifest_path),
        "test_sha256": digest(Path(__file__)),
        "helper_sha256": digest(ROOT / "test/e2e/local_browser_fixture.py"),
        "runtime_fixture_sha256": digest(ROOT / "test/e2e/playground_gateway_fixture.py"),
        "execution": "real Docker container running the audited fake-engine image; the Playground streams a real gateway request; GPU facts are injected only for scheduling",
        "runtime": "real engine SSE and gateway path; no real GPU compute, dGPU isolation or vendor driver",
        "not_covered": ["streaming cancellation and upstream termination",
                        "compare mode, attachments, parameters and other renderers",
                        "gateway errors (unknown model, unauthorized key, engine failure)",
                        "conversation rename/delete and the conversation cap",
                        "the other locale (each run covers one --locale)"],
        "command": ["python3", "test/e2e/playground_chat_gateway.py", *sys.argv[1:]]}
    server = LocalServer(args.server_bin, artifacts.private)
    holder = {"agent": None, "log": None}
    token = None
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["agent_binary_sha256"] = digest(args.agent_bin)
        agent_bin = args.agent_bin.resolve()
        report["agent_bin"] = str(agent_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        status, login = http(server.base, "POST", "/api/v1/auth/login",
                             body={"username": "admin", "password": "password"})
        check(status == 200, f"fixture login HTTP {status}")
        token = login["token"]
        artifacts.remember(token)
        report["preflight"] = "PASS"
        node, node_token, model, deployment = create_fixtures(
            server, token, run_id, report, holder, agent_bin, artifacts,
            inject_online_failure=args.inject_online_failure,
            inject_model_failure=args.inject_model_failure,
            inject_config_failure=args.inject_config_failure,
            inject_deployment_failure=args.inject_deployment_failure)
        artifacts.write_json("report.json", report)
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"],
                locale="zh-CN" if args.locale == "zh" else "en-US", timezone_id="UTC")
            report["browser_version"] = browser.version
            page = context.new_page()
            page.set_default_timeout(10000)
            try:
                test_playground_chat(page, context, server, artifacts, report,
                                     args.locale, deployment, run_id)
                layers = {item["layer"] for item in report["evidence"]}
                check(set(manifest["required_evidence"]) - {"cleanup"} <= layers,
                      "required evidence is missing")
                report["case_completed"] = True
            except Exception:
                report["status"] = "FAIL_TEST"
                report["failure"] = artifacts.redact(traceback.format_exc())
                artifacts.write_json("report.json", report)
                page.screenshot(path=artifacts.directory / "failure.png")
            finally:
                artifacts.write_json("report.json", report)
                close_ok = True
                try:
                    context.close()
                except Exception:
                    close_ok = False
                    report["context_close_error"] = artifacts.redact(
                        traceback.format_exc())
                try:
                    browser.close()
                except Exception:
                    close_ok = False
                    report["browser_close_error"] = artifacts.redact(
                        traceback.format_exc())
                report["browser_close_ok"] = close_ok
                if not close_ok:
                    report["status"] = "FAIL_TEST"
                    report.setdefault("failure", "the browser did not close cleanly")
    except Exception:
        report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
        report["failure"] = artifacts.redact(traceback.format_exc())
    finally:
        cleanup_ok = False
        try:
            cleanup_ok = cleanup(server, token, report, holder)
        except Exception:
            report["cleanup_error"] = artifacts.redact(traceback.format_exc())
        try:
            report["server_stopped"] = server.stop()
        except Exception:
            report["server_stopped"] = False
            report["server_stop_error"] = artifacts.redact(traceback.format_exc())
        cleanup_ok = cleanup_ok and report["server_stopped"]
        browser_ok = report.get("browser_close_ok", True)
        cleanup_ok = cleanup_ok and browser_ok
        report["environment_cleanup_ok"] = cleanup_ok
        report["evidence"].append({"layer": "cleanup", "ok": cleanup_ok,
                                   "remaining_resources": report.get("remaining_resources", [])})
        layers = {item["layer"] for item in report["evidence"]}
        if (report.get("case_completed") and cleanup_ok
                and report.get("status") not in ("FAIL_TEST", "BLOCKED_INFRA")
                and set(manifest["required_evidence"]) <= layers):
            report["status"] = "PASS"
        elif report["status"] not in ("FAIL_TEST", "BLOCKED_INFRA"):
            report["status"] = "FAIL_TEST"
            report.setdefault("failure",
                              "cleanup or required evidence did not complete")
        server_log = artifacts.private / "server.log"
        if server_log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(server_log.read_text()))
        agent_log_path = artifacts.private / "agent.log"
        if agent_log_path.exists():
            (artifacts.directory / "agent.log").write_text(artifacts.redact(agent_log_path.read_text()))
        if not report["server_stopped"]:
            report.setdefault("remaining_resources", []).append({
                "kind": "server", "id": getattr(server.process, "pid", "unknown"),
                "state": "running or unknown", "reclaim": "stop the owned server PID"})
        report["private_state"] = (
            "private/ retains isolated DB/logs and agent state; inspect remaining_resources for scoped recovery (7 days)"
            if report.get("remaining_resources") else
            "private/ retains isolated DB/logs and agent state; no process or container kept (7 days)")
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
        except Exception:
            report["status"] = "FAIL_TEST"
            report["artifact_error"] = "credential scan rejected artifacts; private directory must not be uploaded"
            artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": CONTRACT, "run_id": run_id,
                      "report": str(artifacts.directory / "report.json")}, ensure_ascii=False))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
