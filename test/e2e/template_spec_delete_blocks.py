"""Deployment pin and component reference that make a user-spec delete return 409."""
from __future__ import annotations

import json
import sqlite3
from urllib.parse import parse_qs, urlsplit

from playwright.sync_api import expect
from platform_token_read_revoke import button, check

API = "/api/v1/inference-templates"
PIN_ERROR = "inference catalog user spec is still referenced by deployments"
COMPONENT_ERROR = "inference model is still referenced as a component"


def _db(server):
    conn = sqlite3.connect(server.work / "server-data" / "gpuworks.db", timeout=5)
    conn.execute("PRAGMA busy_timeout=5000")
    return conn


def _count(server, sql, args):
    conn = _db(server)
    try:
        return conn.execute(sql, args).fetchone()[0]
    finally:
        conn.close()


def _pin(server, template_id, spec_name, name):
    conn = _db(server)
    try:
        cur = conn.execute(
            "INSERT INTO deployments (name, template_id, template_spec_source, template_spec_name, desired_state) "
            "VALUES (?, ?, 'user', ?, 'active')",
            (name, template_id, spec_name))
        dep_id = cur.lastrowid
        conn.commit()
    finally:
        conn.close()
    check(_count(server, "SELECT COUNT(*) FROM deployments WHERE id = ? AND template_id = ? AND template_spec_source = ? "
                 "AND template_spec_name = ? AND desired_state = 'active'",
                 (dep_id, template_id, "user", spec_name)) == 1, "spec pin missing after insert")
    return dep_id


def _release_pin(server, dep_id):
    """A deployment whose desired state is deleting no longer pins the spec."""
    conn = _db(server)
    try:
        cur = conn.execute(
            "UPDATE deployments SET desired_state = 'deleting' WHERE id = ? AND desired_state = 'active'",
            (dep_id,))
        conn.commit()
        check(cur.rowcount == 1, "spec pin was not the inserted active deployment")
    finally:
        conn.close()
    check(_count(server, "SELECT COUNT(*) FROM deployments WHERE id = ? AND desired_state = 'deleting'",
                 (dep_id,)) == 1, "released spec pin disappeared")


def _mount(server, target_model_id):
    owner_model_id = "e2e-ref-owner"
    conn = _db(server)
    try:
        cur = conn.execute(
            "INSERT INTO inference_models (model_id, source) VALUES (?, 'upstream')",
            (owner_model_id,))
        owner_id = cur.lastrowid
        spec = conn.execute(
            "INSERT INTO inference_specs (model_id, source, name, image, weight_sources, gpus, components) "
            "VALUES (?, 'upstream', 'owner-spec', 'owner/image:test', '[]', '{}', ?)",
            (owner_id, json.dumps([{"model_id": target_model_id}])))
        conn.commit()
        spec_id = spec.lastrowid
    finally:
        conn.close()
    check(_count(server, "SELECT COUNT(*) FROM inference_specs WHERE id = ? AND components LIKE ?",
                 (spec_id, "%" + target_model_id + "%")) == 1, "component reference missing after insert")
    return owner_model_id, spec_id


def _clear_mount(server, spec_id):
    conn = _db(server)
    try:
        cur = conn.execute("UPDATE inference_specs SET components = '[]' WHERE id = ? AND components <> '[]'", (spec_id,))
        conn.commit()
        check(cur.rowcount == 1, "component reference was not the inserted spec")
    finally:
        conn.close()


def _refuse(page, confirm, ok_label, label):
    with page.expect_response(lambda r: urlsplit(r.url).path == API + "/spec" and r.request.method == "DELETE") as received:
        button(confirm, ok_label).click()
    response = received.value
    check(response.status == 409, f"{label} status {response.status} {response.text()}")
    check(response.request.post_data is None, f"{label} carried a body")
    return response


def exercise_spec_delete_blocks(page, server, editor, spec_name, read, open_confirm):
    """Block the last user spec twice, then leave both blocks clear for the real delete."""
    baseline = read()
    model_id, template_id = baseline["model_id"], baseline["id"]
    pin_name = "e2e-spec-pin"
    dep_id = _pin(server, template_id, spec_name, pin_name)
    try:
        pinned = _refuse(page, open_confirm(), editor["deleteSpecOk"], "reference block")
        pin_error = pinned.json().get("error", "")
        check(pin_error.startswith(PIN_ERROR) and pin_name in pin_error, f"reference block error {pin_error}")
        check(parse_qs(urlsplit(pinned.url).query) == {"model_id": [model_id], "spec": [spec_name]},
              "reference block targeted another spec")
        expect(page.locator(".ant-message")).to_contain_text(pin_name)
        check(read() == baseline, "reference block changed the model")
        check(_count(server, "SELECT COUNT(*) FROM deployments WHERE id = ?", (dep_id,)) == 1,
              "reference block removed the deployment")
        _release_pin(server, dep_id)
        dep_id = None
        owner_model_id, spec_id = _mount(server, model_id)
        mounted = _refuse(page, open_confirm(), editor["deleteSpecOk"], "component block")
        component_error = mounted.json().get("error", "")
        check(component_error.startswith(COMPONENT_ERROR) and owner_model_id in component_error and model_id in component_error,
              f"component block error {component_error}")
        expect(page.locator(".ant-message")).to_contain_text(owner_model_id)
        check(read() == baseline, "component block changed the model")
        _clear_mount(server, spec_id)
        return {"layer": "ui", "delete_confirmation": editor["deleteSpecBody"].replace("{{name}}", spec_name),
                "reference_block_status": 409, "reference_block_error": pin_error,
                "component_block_status": 409, "component_block_error": component_error,
                "model_unchanged": True}
    finally:
        if dep_id is not None:
            _release_pin(server, dep_id)
