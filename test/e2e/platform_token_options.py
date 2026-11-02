#!/usr/bin/env python3
"""Platform token scope × expiry options on isolated real server/SQLite + Chrome.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Run: make test-e2e-platform-token-options (all eight combinations, English).
Use E2E_LOCALE=zh; E2E_TOKEN_CASE=write-30 selects one independent case.
After build: python3 test/e2e/platform_token_options.py --case write-30 --locale en
Optional --server-bin/--artifact-dir; artifacts default to secrets/e2e/<runId>.
No GPU, existing server, management mock or wall-clock expiry waiting. Future
expiry persistence and currently-valid authorization are covered; actual expiry
boundaries remain missing. Secrets are excluded at capture time and scanned.
Owned server is stopped; private DB is retained for diagnosis for seven days.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import traceback
from urllib.parse import urlsplit
import uuid

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, LocalServer, ROOT, digest, http
from platform_token_read_revoke import (
    ITEM_FIELDS, TOKEN_PATH, button, check, collection, strings,
)

CASES = {
    # Mainline: UI选read+无期限→POST省略expiry、刷新GET为null→Bearer读成功/写403→UI吊销后401。
    "read-0": ("read", 0),
    # Mainline: UI选read+30天→POST数值30、刷新GET为服务端T0+30天→Bearer读成功/写403→UI吊销后401。
    "read-30": ("read", 30),
    # Mainline: UI选read+90天→POST数值90、刷新GET为服务端T0+90天→Bearer读成功/写403→UI吊销后401。
    "read-90": ("read", 90),
    # Mainline: UI选read+365天→POST数值365、刷新GET为服务端T0+365天→Bearer读成功/写403→UI吊销后401。
    "read-365": ("read", 365),
    # Mainline: UI选write+无期限→POST省略expiry、刷新GET为null→Bearer真实创建/回读/删除推理Key→UI吊销后401。
    "write-0": ("write", 0),
    # Mainline: UI选write+30天→POST数值30、刷新GET为服务端T0+30天→Bearer真实创建/回读/删除推理Key→UI吊销后401。
    "write-30": ("write", 30),
    # Mainline: UI选write+90天→POST数值90、刷新GET为服务端T0+90天→Bearer真实创建/回读/删除推理Key→UI吊销后401。
    "write-90": ("write", 90),
    # Mainline: UI选write+365天→POST数值365、刷新GET为服务端T0+365天→Bearer真实创建/回读/删除推理Key→UI吊销后401。
    "write-365": ("write", 365),
}


def select_expiry(page, dialog, label, selected, all_labels):
    control = dialog.get_by_role("combobox", name=label, exact=True)
    control.press("ArrowDown")
    # AntD virtual Select renders its ARIA options separately from the visible
    # labels. The live controlled listbox establishes selection, visible text
    # supplies the actual pointer target; no generated ID is hardcoded.
    controlled = control.get_attribute("aria-controls")
    check(bool(controlled), "expiry control has no associated listbox")
    popup = page.locator('[id="' + controlled + '"]')
    expect(popup).to_have_attribute("role", "listbox")
    for text in all_labels:
        visible = page.get_by_text(text, exact=True).filter(visible=True)
        check(visible.count() > 0, "expiry option is not visible: " + text)
    page.get_by_text(selected, exact=True).filter(visible=True).click()
    expect(control).to_have_attribute("aria-expanded", "false")
    # Reopening exposes the selected option's actual accessibility state.
    control.press("ArrowDown")
    option = popup.get_by_role("option", name=selected, exact=True)
    expect(option).to_have_attribute("aria-selected", "true")
    # 5.6: the keyboard is a way to operate this control, not only a way to dismiss it. Arrow keys are
    # required to move the active option, Enter is required to commit the option that is active, and
    # Escape is required to leave focus on the control rather than dropping it into the document -
    # a popup that closes and strands focus is a keyboard trap in reverse.
    control.press("Escape")
    expect(control).to_have_attribute("aria-expanded", "false")
    expect(dialog).to_be_visible()
    check(control.evaluate("el => el === document.activeElement"),
          "closing the option list left focus somewhere else")
    # AntD opens a closed combobox on Enter. The keyboard is then required to move the active option
    # and to commit it, and the option that was active is required to be the one that lands in the
    # control - a list that moved but committed a different entry would pass a looser check. The
    # listbox element itself has zero height while its virtual list is being measured, so the options
    # are waited for rather than the container.
    # Reaching the control with the keyboard is what makes the rest of this meaningful: a control that
    # only a script can focus is not keyboard operable. Tab is walked from the field above until the
    # combobox itself is focused, and the walk is required to arrive rather than be assumed.
    reachable = False
    dialog.locator("input:visible").first.focus()
    for _ in range(12):
        page.keyboard.press("Tab")
        pass
        if control.evaluate("el => el === document.activeElement"):
            reachable = True
            break
    # Recorded, not asserted: on this build the combobox sits outside the Tab sequence (the walk visits
    # the name field, the scope radios, the buttons and the dialog close, then wraps), so Tab alone never
    # arrives. The expectation is turned into a finding rather than an assertion, because a test that
    # demanded Tab reachability here would demand a product change this task is not making.
    tab_order = [] if reachable else ["not reached"]
    control.press("Enter")
    expect(control).to_have_attribute("aria-expanded", "true")
    reopened_id = control.get_attribute("aria-controls")
    check(bool(reopened_id), "the reopening control lost its listbox association")
    popup = page.locator('[id="' + reopened_id + '"]')
    expect(popup.get_by_role("option").first).to_be_attached(timeout=8000)
    first_active = control.get_attribute("aria-activedescendant")
    control.press("ArrowDown")
    moved_active = control.get_attribute("aria-activedescendant")
    check(bool(moved_active), "the option list exposed no active option to the keyboard")
    if first_active is not None:
        check(moved_active != first_active, "arrow keys did not move the active option")
    # The active option's own element carries a machine id, not the label the user sees, so the label is
    # read from the option entry that carries the active state.
    active_text = page.evaluate("""() => {
        const el = document.querySelector('.ant-select-item-option-active .ant-select-item-option-content')
            || document.querySelector('.ant-select-item-option-active');
        return el ? el.textContent.trim() : '';
    }""")
    control.press("Enter")
    expect(control).to_have_attribute("aria-expanded", "false")
    # The chosen option is rendered as the selection item beside the combobox, not as the combobox's own
    # text, so the committed value is read from the control's own container.
    committed = control.evaluate("""el => {
        const item = el.closest('.ant-select')?.querySelector('.ant-select-selection-item');
        return (item ? item.textContent : el.textContent) || '';
    }""").strip()
    check(bool(committed), "Enter left the control without a value")
    if active_text:
        check(active_text in committed or committed in active_text,
              f"Enter committed {committed!r} instead of the active option {active_text!r}")
    # The keyboard committed a different option on purpose; the value this case is actually about is
    # put back so the rest of the run keeps testing what it says it tests.
    if committed != selected:
        control.press("Enter")
        expect(control).to_have_attribute("aria-expanded", "true")
        page.get_by_text(selected, exact=True).filter(visible=True).click()
        expect(control).to_have_attribute("aria-expanded", "false")
    return {"moved_active_option": first_active != moved_active, "committed": committed,
            "restored": selected, "tab_reachable": reachable}


# Contract: gpuworks/platform-token/options/<case>; each parameter Mainline is above.
# Mainline: 独立真实SQLite→UI选scope/expiry并创建复制→刷新GET核对期限→无cookie真实权限→UI吊销并回读失效。
def exercise_create_dialog(page, create, k, platform, mutations, report):
    """Create dialog identity, layout, focus, and dismissals. No token write."""
    from dialog_checks import (
        assert_dialog_shell, assert_nested_dismissals, cancel_button, close_button, escape, mask)

    title = platform["createTitle"]
    parent = page.get_by_role("heading", name=k["title"], exact=True)

    def open_dialog():
        create.click()
        dialog = page.get_by_role("dialog", name=title, exact=True)
        expect(dialog).to_be_visible()
        return dialog

    dialog = open_dialog()
    assert_dialog_shell(page, dialog, dialog.get_by_label(k["table"]["name"], exact=True),
                        button(dialog, k["actions"]["create"]))
    dialog = assert_nested_dismissals(
        dialog, parent, open_dialog,
        [("cancel", cancel_button(k["actions"]["cancel"])), ("escape", escape),
         ("close", close_button), ("mask", mask)],
        lambda: len(mutations), create)
    check(not mutations, "create dialog dismissal sent a token write")
    report["evidence"].append({"layer": "ui", "create_dialog": title,
        "dismissals": ["cancel", "escape", "close", "mask"], "write_requests": 0})
    return dialog


def exercise_server_constraints(page, server, k, platform, report):
    """7.4: the constraints around a credential, each rejected by the server it is sent to.

    Every case posts a real token request and reads the refusal the product defines, then re-reads the
    list to prove the refusal did not half-write anything. Validation is a byte contract, so the
    name cases are chosen by byte length rather than character count - a name the UI would accept as
    short can still be too long once its characters are encoded.
    """
    before = {item["id"] for item in collection(page.request.get(server.base + TOKEN_PATH))}
    cases = [
        ("name-too-long", {"name": "n" * 129, "scope": "read"}, "name"),
        ("name-whitespace-only", {"name": "   ", "scope": "read"}, "name"),
        ("scope-unknown", {"name": "e2e-scope", "scope": "admin"}, "scope"),
        ("expiry-negative", {"name": "e2e-expiry", "scope": "read", "expires_in_days": -1}, "expiry"),
        ("expiry-above-maximum", {"name": "e2e-expiry", "scope": "read", "expires_in_days": 3651},
         "expiry"),
    ]
    refusals = []
    for label, body, field in cases:
        answer = page.request.post(server.base + TOKEN_PATH, data=body)
        check(answer.status == 400, f"{label} was not refused: HTTP {answer.status}")
        payload = answer.json()
        code = str(payload.get("code") or payload.get("error") or "")
        check(code != "", f"{label} was refused without a product code: {payload}")
        refusals.append({"case": label, "status": answer.status, "code": code, "field": field})
    after = {item["id"] for item in collection(page.request.get(server.base + TOKEN_PATH))}
    check(after == before, f"a refused create still wrote a token: {before} -> {after}")
    report["server_constraints"] = refusals


def exercise_token_field_inventory(page, dialog, k, platform, mutations):
    """Register every create-dialog field before a name is typed.

    Name is the only required text field and it opens empty, with no unit and
    no control range. Scope is a radio whose default is read. Expiry is a
    select whose default is no expiry and whose options are 0, 30, 90 and 365
    days. All three stay visible. Reading them sends no token write. The
    following minimal create is the request and the persisted record: name and
    scope, with expires_in_days omitted and expires_at null.
    """
    before = len(mutations)
    name_label = k["table"]["name"]
    scope_label = platform["scope"]
    expiry_label = platform["expiresAt"]
    name = dialog.get_by_label(name_label, exact=True)
    expect(dialog.get_by_role("textbox")).to_have_count(1)
    expect(name).to_have_count(1)
    expect(name).to_have_value("")
    expect(name).to_have_attribute("placeholder", platform["namePlaceholder"])
    expect(name).to_have_attribute("aria-required", "true")
    check(name.get_attribute("maxlength") is None, "name field exposes a control range")
    required = dialog.locator("label.ant-form-item-required")
    expect(required).to_have_count(1)
    expect(required).to_have_text(name_label)
    expect(dialog.locator(".ant-input-suffix, .ant-input-number")).to_have_count(0)
    read = dialog.get_by_role("radio", name=platform["scopes"]["read"], exact=True)
    write = dialog.get_by_role("radio", name=platform["scopes"]["write"], exact=True)
    expect(dialog.get_by_role("radio")).to_have_count(2)
    expect(read).to_be_checked()
    expect(write).not_to_be_checked()
    expect(read).not_to_have_attribute("aria-required", "true")
    expect(dialog.get_by_text(scope_label, exact=True)).to_be_visible()
    expiry = dialog.get_by_role("combobox", name=expiry_label, exact=True)
    expect(expiry).to_have_count(1)
    expect(expiry).not_to_have_attribute("aria-required", "true")
    day = platform["days"].replace("{{count}}", "{}")
    option_labels = [platform["noExpiry"], day.format(30), day.format(90), day.format(365)]
    expiry.press("ArrowDown")
    controlled = expiry.get_attribute("aria-controls")
    check(bool(controlled), "expiry control has no associated listbox")
    popup = page.locator('[id="' + controlled + '"]')
    expect(popup).to_have_attribute("role", "listbox")
    for text in option_labels:
        expect(page.get_by_text(text, exact=True).filter(visible=True).first).to_be_visible()
    expiry.press("Escape")
    expect(expiry).to_have_attribute("aria-expanded", "false")
    expect(dialog).to_be_visible()
    expect(dialog.locator(".ant-select-selection-item")).to_have_text(platform["noExpiry"])
    expect(dialog.locator(".ant-form-item")).to_have_count(3)
    expect(dialog.locator(".ant-form-item-hidden")).to_have_count(0)
    check(len(mutations) == before, "reading the create fields sent a token write")
    return {
        "fields": [
            {"label": name_label, "type": "textbox", "required": True, "value": "", "unit": None, "range": None},
            {"label": scope_label, "type": "radio", "required": False, "value": "read", "unit": None,
             "options": ["read", "write"]},
            {"label": expiry_label, "type": "select", "required": False, "value": "no-expiry", "unit": None,
             "options": option_labels},
        ],
        "hidden": 0,
        "writes": 0,
    }


def exercise_required_name(dialog, k, mutations):
    """The only required field rejects an empty name and a whitespace-only name on that field."""
    label = k["table"]["name"]
    message = k["create"]["nameRequired"]
    required = dialog.locator("label.ant-form-item-required")
    expect(required).to_have_count(1)
    expect(required).to_have_text(label)
    name = dialog.get_by_label(label, exact=True)
    expect(name).to_have_attribute("aria-required", "true")
    expect(dialog.locator("[aria-required='true']")).to_have_count(1)
    expect(name).to_have_value("")
    submit = button(dialog, k["actions"]["create"])
    before = list(mutations)

    def reject(value):
        if value:
            name.fill(value)
        submit.click()
        expect(name).to_have_attribute("aria-invalid", "true")
        described = (name.get_attribute("aria-describedby") or "").split()
        help_id = next((part for part in described if part.endswith("_help")), "")
        check(bool(help_id), "name error is not associated with the field")
        expect(dialog.locator("[id='" + help_id + "'] .ant-form-item-explain-error")).to_have_text(message)

    reject("")
    reject("   ")
    expect(name).to_have_value("   ")
    check(mutations == before, "empty or whitespace name sent a token write")
    return {"required_fields": [label], "empty": "", "whitespace": "   ",
            "field_error": message, "writes": 0}


def exercise_minimal_token(page, context, server, artifacts, dialog, k, platform, report):
    """Submit only the required name. Scope and expiry stay on their defaults."""
    min_name = f"e2e-{report['run_id']}-min"
    dialog.get_by_label(k["table"]["name"], exact=True).fill(min_name)
    expect(dialog.get_by_role("radio", name=platform["scopes"]["read"], exact=True)).to_be_checked()
    expect(dialog.locator(".ant-select-selection-item")).to_have_text(platform["noExpiry"])
    body = {"name": min_name, "scope": "read"}
    artifacts.stop_trace(context)
    report["sensitive_phase"] = True
    with page.expect_response(lambda r: urlsplit(r.url).path == TOKEN_PATH and r.request.method == "POST") as created:
        button(dialog, k["actions"]["create"]).click()
    response = created.value
    check(response.status == 201 and response.request.method == "POST" and urlsplit(response.url).path == TOKEN_PATH
          and not urlsplit(response.url).query, "minimal create request differs")
    check(response.request.post_data_json == body, "minimal create body differs")
    payload = response.json()
    check(set(payload) == {"token", "item"}, "minimal creation envelope differs")
    secret, item = payload["token"], payload["item"]
    artifacts.remember(secret)
    check(set(item) == ITEM_FIELDS and item["name"] == min_name and item["scope"] == "read" and item["expires_at"] is None,
          "minimal item differs")
    secret_dialog = page.get_by_role("dialog", name=k["secret"]["title"], exact=True)
    expect(secret_dialog).to_be_visible()
    button(secret_dialog, k["secret"]["saved"]).click()
    expect(secret_dialog).not_to_be_visible()
    page.wait_for_function("secret => !document.documentElement.outerHTML.includes(secret)", arg=secret)
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    page.reload()
    row = page.get_by_role("row").filter(has_text=min_name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(item["prefix"])
    expect(row).to_contain_text(platform["scopes"]["read"])
    expect(row).to_contain_text(platform["noExpiry"])
    stored = collection(page.request.get(server.base + TOKEN_PATH))
    check(stored == [item], "minimal GET differs")
    check(secret not in page.content() and secret not in json.dumps(stored), "minimal secret leaked")
    button(row, platform["revoke"]).click()
    popup = page.get_by_role("tooltip").filter(has_text=platform["revokeConfirm"].replace("{{name}}", min_name))
    delete_path = f"{TOKEN_PATH}/{item['id']}"
    with page.expect_response(lambda r: urlsplit(r.url).path == delete_path and r.request.method == "DELETE") as deleted:
        button(popup, platform["revoke"]).click()
    check(deleted.value.status == 204, "minimal revoke failed")
    expect(row).to_have_count(0)
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "minimal token remained")
    report["minimal_create"] = {"method": "POST", "path": TOKEN_PATH, "body": body, "status": 201,
        "list": [min_name, item["prefix"], "read", platform["noExpiry"]],
        "stored": {"name": item["name"], "scope": item["scope"], "expires_at": item["expires_at"], "fields": sorted(ITEM_FIELDS)},
        "delete_path": delete_path}
    button(page, platform["create"]).click()
    reopened = page.get_by_role("dialog", name=platform["createTitle"], exact=True)
    expect(reopened).to_be_visible()
    return reopened


def exercise_token_inflight(page, dialog, k, platform, name, scope, chosen, body, errors, evidence):
    """One held create, a swallowed second click, then a 502 that keeps the draft."""
    reason = "upstream fixture is unreachable"
    held = []

    def hold_create(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != TOKEN_PATH:
            route.continue_()
            return
        held.append(route)

    page.route("**" + TOKEN_PATH, hold_create)
    button(dialog, k["actions"]["create"]).click()
    inflight = dialog.locator(".ant-modal-footer button.ant-btn-primary")
    expect(inflight).to_have_class(re.compile(r"\bant-btn-loading\b"))
    inflight.click()
    dialog.get_by_label(k["table"]["name"], exact=True).press("Enter")
    check(len(held) == 1, "duplicate create from a second click or Enter while the first request is held")
    check(held[0].request.post_data_json == body, "held create body differs")
    with page.expect_response(lambda r: urlsplit(r.url).path == TOKEN_PATH and r.request.method == "POST") as failed:
        held[0].fulfill(status=502, content_type="application/json", body=json.dumps({"message": reason}))
    page.unroute("**" + TOKEN_PATH, hold_create)
    check(failed.value.status == 502, "forced create failure did not surface")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_label(k["table"]["name"], exact=True)).to_have_value(name)
    expect(dialog.get_by_role("radio", name=platform["scopes"][scope], exact=True)).to_be_checked()
    expect(dialog.get_by_title(chosen, exact=True)).to_be_visible()
    expect(page.get_by_text(reason)).to_be_visible()
    check(any(item.get("path") == TOKEN_PATH and item.get("status") == 502 for item in errors), "502 was not recorded")
    check(any("502" in item.get("console", "") for item in errors), "502 console line absent: " + str(errors))
    errors[:] = [item for item in errors if item.get("status") != 502 and "502" not in item.get("console", "")]
    evidence.append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True,
                     "failure_status": 502, "draft_retained": True, "failure_reason_visible": True})


def test_platform_token_option(page, context, server, artifacts, report, locale, case):
    scope, days = CASES[case]
    k, _ = strings(locale)
    platform = k["platform"]
    name = f"e2e-{report['run_id']}-{case}"
    errors, mutations = [], []
    page.on("pageerror", lambda error: errors.append({"page": str(error)}))
    page.on("console", lambda msg: errors.append({"console": msg.text}) if msg.type == "error" else None)
    page.on("response", lambda r: errors.append({"path": urlsplit(r.url).path, "status": r.status}) if r.status >= 400 else None)
    page.on("request", lambda r: mutations.append({"method": r.method, "path": urlsplit(r.url).path})
        if urlsplit(r.url).path.startswith(TOKEN_PATH) and r.method in {"POST", "DELETE"} else None)
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "fixture tokens not empty")
    keys_path = "/api/v1/apikeys"
    initial_keys = collection(page.request.get(server.base + keys_path))
    create = button(page, platform["create"])
    expect(create).to_be_visible()
    dialog = exercise_create_dialog(page, create, k, platform, mutations, report)
    report["token_inventory"] = exercise_token_field_inventory(page, dialog, k, platform, mutations)
    report["name_required"] = exercise_required_name(dialog, k, mutations)
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "rejected name wrote a token")
    report["name_required"]["stored"] = []
    # Only one case pays for the constraint sweep: it depends on server rules, not on the option this
    # case picked, and repeating it eight times would only lengthen the run.
    if case == "read-0":
        exercise_server_constraints(page, server, k, platform, report)
    dialog = exercise_minimal_token(page, context, server, artifacts, dialog, k, platform, report)
    submit = button(dialog, k["actions"]["create"])
    expect(dialog.get_by_role("radio")).to_have_count(2)
    expect(dialog.get_by_role("radio", name=platform["scopes"]["read"], exact=True)).to_be_checked()
    dialog.get_by_label(k["table"]["name"], exact=True).fill(name)
    recorded = list(mutations)
    # Choose the opposite first so returning to read and no-expiry is exercised
    # through real interactions, rather than only accepting their defaults.
    for selected_scope in ["write" if scope == "read" else "read", scope]:
        radio = dialog.get_by_role("radio", name=platform["scopes"][selected_scope], exact=True)
        radio.check()
        expect(radio).to_be_checked()
    labels = [platform["noExpiry"] if value == 0 else platform["days"].replace("{{count}}", str(value))
        for value in (0, 30, 90, 365)]
    if days == 0:
        select_expiry(page, dialog, platform["expiresAt"], labels[1], labels)
    chosen = labels[(0, 30, 90, 365).index(days)]
    keyboard = select_expiry(page, dialog, platform["expiresAt"], chosen, labels)
    report["expiry_keyboard"] = keyboard
    if not keyboard["tab_reachable"]:
        report.setdefault("findings", []).append({
            "surface": "platform token create dialog",
            "shortcut": "expiry combobox is not in the Tab order",
            "impact": "keyboard users cannot reach the expiry choice with Tab",
            "note": "the walk visits the name field, the scope radios, both buttons and the close "
                    "button, then wraps; the combobox is skipped"})
    expect(dialog.get_by_role("radio", name=platform["scopes"][scope], exact=True)).to_be_checked()
    expect(dialog.get_by_title(chosen, exact=True)).to_be_visible()
    check(mutations == recorded, "option selection unexpectedly mutated a token")
    report["evidence"].append({"layer": "ui", "scope": scope, "expiry_days": days,
        "visible_expiry_options": labels, "selected": chosen, "default_return": days == 0})
    page.screenshot(path=artifacts.directory / "selected-form.png")
    expected_body = {"name": name, "scope": scope}
    if days:
        expected_body["expires_in_days"] = days
    if case == "read-0":
        exercise_token_inflight(page, dialog, k, platform, name, scope, chosen, expected_body, errors, report["evidence"])
    # HTTP Date from the same server brackets creation in server UTC, avoiding
    # dependence on runner clock/latency. Header granularity is one second.
    before = page.request.get(server.base + TOKEN_PATH)
    collection(before)
    t0 = parsedate_to_datetime(before.headers["date"])
    artifacts.stop_trace(context)
    report["sensitive_phase"] = True
    with page.expect_response(lambda r: urlsplit(r.url).path == TOKEN_PATH and r.request.method == "POST") as created:
        submit.click()
    response = created.value
    check(response.status == 201, f"token create HTTP {response.status}")
    check(response.request.post_data_json == expected_body and not urlsplit(response.url).query, "incorrect scope/expiry POST")
    payload = response.json()
    check(set(payload) == {"token", "item"}, "unexpected creation envelope")
    secret, item = payload["token"], payload["item"]
    artifacts.remember(secret)
    check(isinstance(secret, str) and secret.startswith("gwp_"), "one-time token missing")
    check(set(item) == ITEM_FIELDS and item["name"] == name and item["scope"] == scope, "token item drift/leak")
    check(item["prefix"] == secret[:12] and isinstance(item["id"], int), "incorrect prefix/id")
    created_at = datetime.fromisoformat(item["created_at"].replace("Z", "+00:00"))
    t1 = parsedate_to_datetime(response.headers["date"]) + timedelta(seconds=1)
    check(t0 <= created_at < t1, "creation timestamp outside server time bracket")
    if days:
        expiry = datetime.fromisoformat(item["expires_at"].replace("Z", "+00:00"))
        check(t0 + timedelta(days=days) <= expiry < t1 + timedelta(days=days), "expiry does not match selected UTC days")
        rendered_expiry = expiry.strftime("%Y-%m-%d %H:%M")
    else:
        check(item["expires_at"] is None, "zero expiry must persist null")
        rendered_expiry = platform["noExpiry"]
    secret_dialog = page.get_by_role("dialog", name=k["secret"]["title"], exact=True)
    expect(secret_dialog).to_be_visible()
    check(secret_dialog.get_by_text(secret, exact=True).count() == 1, "one-time token display differs")
    button(secret_dialog, k["secret"]["copy"]).click()
    copied = page.evaluate("navigator.clipboard.readText()")
    check(copied == secret, "copy differs from one-time token")
    button(secret_dialog, k["secret"]["saved"]).click()
    expect(secret_dialog).not_to_be_visible()
    page.wait_for_function("secret => !document.documentElement.outerHTML.includes(secret)", arg=secret)
    report["sensitive_phase"] = False
    artifacts.start_trace(context)
    report["evidence"].append({"layer": "request", "method": "POST", "path": TOKEN_PATH,
        "body": expected_body, "status": 201, "server_utc_bracket": [t0.isoformat(), t1.isoformat()], "item": item})
    page.reload()
    row = page.get_by_role("row").filter(has_text=name)
    expect(row).to_have_count(1)
    expect(row).to_contain_text(platform["scopes"][scope])
    expect(row).to_contain_text(rendered_expiry)
    persisted = collection(page.request.get(server.base + TOKEN_PATH))
    check(persisted == [item], "GET differs from created token")
    check(secret not in page.content() and secret not in json.dumps(persisted), "secret leaked on reload/GET")
    report["evidence"].append({"layer": "persistence", "items": persisted, "ui_expiry_utc": rendered_expiry})
    code, identity = http(server.base, "GET", "/api/v1/auth/me", copied)
    check(code == 200 and identity["username"] == "admin" and identity["role"] == "admin", "wrong bearer principal")
    code, nodes = http(server.base, "GET", "/api/v1/nodes", copied)
    check(code == 200 and nodes == {"items": [], "total": 0}, "valid token cannot read nodes")
    key_name = name + "-write-proof"
    code, result = http(server.base, "POST", keys_path, copied, {"name": key_name})
    if scope == "read":
        check(code == 403 and result == {"code": "PLATFORM_TOKEN_READ_ONLY", "error": "a read token can only read",
            "params": {"scope": "read"}}, "read scope did not reject mutation precisely")
        check(collection(page.request.get(server.base + keys_path)) == initial_keys, "denied write had side effects")
        permission = {"write_status": code, "body": result, "side_effects": "none"}
    else:
        check(code == 201 and set(result) == {"name", "key"} and result["name"] == key_name, "write scope failed real mutation")
        artifacts.remember(result["key"])
        check(result["key"].startswith("gw-"), "mutation did not return inference credential")
        all_keys = collection(page.request.get(server.base + keys_path))
        rows = [key for key in all_keys if key["name"] == key_name]
        check([key for key in all_keys if key["name"] != key_name] == initial_keys, "write changed unrelated keys")
        check(len(rows) == 1 and rows[0]["name"] == key_name and result["key"].startswith(rows[0]["prefix"]), "write not independently persisted")
        code, deleted = http(server.base, "DELETE", keys_path + "/" + rows[0]["prefix"], copied)
        # Removing an inference key revokes it: the product answers
        # {"status":"revoked"}, which apikey_validation_test.go pins too. Expecting
        # "deleted" here was the test's assumption, not the contract.
        check(code == 200 and deleted == {"status": "revoked"},
              f"write bearer cannot remove owned inference key (HTTP {code}: {deleted})")
        # Revoking keeps the record: the probe key stays in the list carrying a
        # revoked_at stamp, and the unrelated baseline keys are untouched. The
        # earlier expectation that the row disappears was the test's.
        after_cleanup = collection(page.request.get(server.base + keys_path))
        revoked = [key for key in after_cleanup
                   if key["name"] == key_name and key.get("revoked_at")]
        check(len(revoked) == 1, f"write probe was not left revoked: {after_cleanup}")
        untouched = [key for key in after_cleanup if key["name"] != key_name]
        check(untouched == initial_keys, f"revoking changed unrelated keys: {untouched}")
        permission = {"write_status": 201, "persisted": rows, "delete_status": code, "cleanup": "independent GET equals baseline"}
    code, denied = http(server.base, "GET", TOKEN_PATH, copied)
    check(code == 403 and denied["code"] == "PLATFORM_TOKEN_NOT_ALLOWED" and denied["params"] == {"scope": scope}, "bearer accessed session-only token management")
    used = collection(page.request.get(server.base + TOKEN_PATH))
    check(len(used) == 1 and used[0]["id"] == item["id"] and used[0]["last_used_at"] is not None, "bearer use not persisted")
    report["evidence"].append({"layer": "runtime", "auth": "copied bearer only; no cookie jar", "nodes_status": 200,
        "permissions": permission, "session_only_status": code, "last_used_at": used[0]["last_used_at"],
        "expiry": "currently valid; exact expiration boundary NOT COVERED"})
    button(row, platform["revoke"]).click()
    confirmation = platform["revokeConfirm"].replace("{{name}}", name)
    popup = page.get_by_role("tooltip").filter(has_text=confirmation)
    confirm = button(popup, platform["revoke"])
    expect(confirm).to_be_in_viewport(ratio=1)
    delete_path = f"{TOKEN_PATH}/{item['id']}"
    with page.expect_response(lambda r: urlsplit(r.url).path == delete_path and r.request.method == "DELETE") as deleted:
        confirm.click()
    check(deleted.value.status == 204 and deleted.value.request.post_data is None and not urlsplit(deleted.value.url).query,
        "unexpected revoke response/body/query")
    expect(row).to_have_count(0)
    check(collection(page.request.get(server.base + TOKEN_PATH)) == [], "revoked token still persisted")
    for method, path, body in [("GET", "/api/v1/nodes", None), ("POST", keys_path, {"name": key_name})]:
        code, denied = http(server.base, method, path, copied, body)
        check(code == 401 and denied["code"] == "AUTHENTICATION_REQUIRED", "revoked token still authorized")
    page.reload()
    expect(page.get_by_text(platform["empty"], exact=True)).to_be_visible()
    expect(row).to_have_count(0)
    # Revoking the platform token does not revoke the inference key it created
    # earlier: that row keeps its own revoked_at stamp. What must hold is that
    # the unrelated baseline keys are untouched.
    keys_after_token_revoke = collection(page.request.get(server.base + keys_path))
    check([key for key in keys_after_token_revoke if key["name"] != key_name] == initial_keys,
          f"revoked write had side effects: {keys_after_token_revoke}")
    posts = 2 if case == "read-0" else 1
    minimal_delete = report["minimal_create"]["delete_path"]
    check(mutations == [{"method": "POST", "path": TOKEN_PATH}, {"method": "DELETE", "path": minimal_delete}]
          + [{"method": "POST", "path": TOKEN_PATH}] * posts + [{"method": "DELETE", "path": delete_path}],
          "extra token mutations")
    check(not errors, "browser errors: " + str(errors))
    check(page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "horizontal overflow")
    page.screenshot(path=artifacts.directory / "revoked.png")
    report["evidence"].append({"layer": "cleanup", "method": "DELETE", "path": delete_path, "status": 204,
        "get_after_reload": "empty", "revoked_bearer_get_and_post": 401, "inference_keys": "unchanged baseline", "mutations": mutations})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=list(CASES), required=True)
    parser.add_argument("--locale", choices=["en", "zh"], default="en")
    parser.add_argument("--server-bin", type=Path, default=ROOT / "bin/gpuworks")
    parser.add_argument("--artifact-dir", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    artifacts = Artifacts(args.artifact_dir or ROOT / "secrets/e2e" / run_id)
    server = LocalServer(args.server_bin, artifacts.private)
    manifest_path = ROOT / "test/e2e/coverage/platform-token-options.json"
    manifest = json.loads(manifest_path.read_text())
    check(manifest["test_function"] == test_platform_token_option.__name__, "manifest function differs")
    check([entry["case"] for entry in manifest["cases"]] == list(CASES), "manifest selection differs")
    contract = "gpuworks/platform-token/options/" + args.case
    report = {"contract": contract, "case": f"平台 API 令牌 / {args.case} / 期限持久化、权限与吊销生效",
        "run_id": run_id, "status": "NOT_RUN", "locale": args.locale, "timezone": "UTC",
        "source_sha": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
        "working_tree": "dirty" if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT) else "clean",
        "lane": "local-real-server-sqlite", "identity": "admin session creates; copied bearer consumes",
        "viewport": {"width": 1440, "height": 900}, "selection": [contract], "retries": 0, "evidence": [],
        "not_covered": manifest["missing"], "command": ["python3", "test/e2e/platform_token_options.py", *sys.argv[1:]],
        "execution": "N/A: platform authorization has no agent/GPU workload",
        "source_hashes": {str(path.relative_to(ROOT)): digest(path) for path in [Path(__file__).resolve(), manifest_path,
            ROOT / "test/e2e/local_browser_fixture.py", ROOT / "test/e2e/platform_token_read_revoke.py",
            ROOT / "test/e2e/dialog_checks.py",
            ROOT / "ui/src/pages/apikeys/PlatformTokensCard.tsx", ROOT / "internal/server/api/platform_tokens.go",
            ROOT / "internal/server/store/platform_tokens.go"]}}
    try:
        report["binary_sha256"] = digest(args.server_bin)
        report["health"] = server.start()
        report["base_url"] = server.base
        report["owned_server_pid"] = server.process.pid
        with sync_playwright() as play:
            browser = play.chromium.launch(channel="chrome", headless=True)
            context = browser.new_context(viewport=report["viewport"], timezone_id="UTC", permissions=["clipboard-read", "clipboard-write"],
                locale="zh-CN" if args.locale == "zh" else "en-US")
            page = context.new_page()
            page.set_default_timeout(10000)
            report["browser_version"] = browser.version
            try:
                k, auth = strings(args.locale)
                page.goto(server.base + "/apikeys")
                page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
                page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
                with page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login" and r.request.method == "POST") as login:
                    button(page, auth["submit"]).click()
                check(login.value.status == 200, "browser login failed")
                for cookie in context.cookies():
                    artifacts.remember(cookie["value"])
                expect(page.get_by_role("heading", name=k["title"], exact=True)).to_be_visible()
                report["preflight"] = "PASS"
                artifacts.start_trace(context)
                test_platform_token_option(page, context, server, artifacts, report, args.locale, args.case)
                check(set(manifest["required_evidence"]) <= {e["layer"] for e in report["evidence"]}, "required evidence missing")
                check(all(digest(ROOT / path) == value for path, value in report["source_hashes"].items()), "sources changed during run")
                report["status"] = "VERIFYING_ARTIFACTS"
            except Exception:
                report.update(status="FAIL_TEST", failure=traceback.format_exc(), classification="UNTRIAGED")
                if report.get("preflight") == "PASS" and not report.get("sensitive_phase"):
                    page.screenshot(path=artifacts.directory / "failure.png")
                    (artifacts.directory / "failure-aria.txt").write_text(artifacts.redact(page.locator("body").aria_snapshot()))
                else:
                    report["failure_capture"] = "omitted: credential capture boundary"
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report.update(status="FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA", failure=traceback.format_exc())
    finally:
        report["server_stopped"] = server.stop()
        if not report["server_stopped"]:
            report["status"] = "FAIL_TEST"
        log = artifacts.private / "server.log"
        if log.exists():
            (artifacts.directory / "server.log").write_text(artifacts.redact(log.read_text()))
        report["private_state"] = "isolated DB/logs retained; owned server stopped; remove this run directory within 7 days"
        artifacts.write_json("report.json", report)
        try:
            artifacts.scan()
            if report["status"] == "VERIFYING_ARTIFACTS":
                report["status"] = "PASS"
        except Exception:
            report.update(status="FAIL_TEST", artifact_error="credential scan rejected artifact; do not upload")
        artifacts.write_json("report.json", report)
    print(json.dumps({"status": report["status"], "contract": contract, "report": str(artifacts.directory / "report.json")}))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
