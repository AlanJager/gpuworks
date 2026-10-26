"""Field inventory for the platform SSH import dialog and the minimum rotate.

One optional textarea. Import stays disabled until the text is not blank.
Reading it sends no PUT. Rotate fills no fields: one dangerous button, one
empty POST, then the card and an independent GET agree after reload.
"""
from urllib.parse import urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import button, check

_PLACEHOLDER = "-----BEGIN OPENSSH PRIVATE KEY-----"


def exercise_ssh_import_inventory(modal, ssh, mutations):
    """Register the import field before a key is submitted."""
    baseline = len(mutations)
    field = modal.get_by_label(ssh["privateKey"], exact=True)
    expect(modal.get_by_role("textbox")).to_have_count(1)
    expect(field).to_be_visible()
    expect(field).to_have_value("")
    expect(field).to_have_attribute("placeholder", _PLACEHOLDER)
    check(field.get_attribute("maxlength") is None, "the private key has a maximum length")
    check(field.get_attribute("aria-required") is None, "the private key is marked required")
    check(modal.locator(".ant-form-item-required, .ant-input-number, .ant-slider").count() == 0,
          "the import dialog has a unit or a required mark")
    submit = button(modal, ssh["import"])
    expect(submit).to_be_disabled()
    field.fill("   ")
    expect(submit).to_be_disabled()
    field.fill("")
    expect(field).to_have_value("")
    expect(submit).to_be_disabled()
    check(len(mutations) == baseline, "reading the import field sent a mutation")
    return {
        "fields": [{
            "label": ssh["privateKey"], "type": "textbox", "required": False,
            "default": "", "unit": None, "max_length": None, "conditional": False,
            "placeholder": _PLACEHOLDER, "request_field": "private_key_pem",
        }],
        "requests": 0,
        "submit_disabled_while_blank": True,
    }


def exercise_ssh_import_conflict(page, modal, card, ssh, base, key_path, rotate_path,
                                 private_pem, previous_fingerprint, imported_fingerprint):
    """Another session rotates the platform key. The open import draft stays, and saving it wins."""
    field = modal.get_by_label(ssh["privateKey"], exact=True)
    check(field.input_value() == private_pem, "the import draft is not the typed key")
    other = page.request.post(base + rotate_path)
    check(other.status == 200, f"the other ssh session was rejected: HTTP {other.status}")
    other_doc = other.json()
    check(not any("private" in key.lower() for key in other_doc),
          "the rotate response exposes a private field")
    other_fingerprint = other_doc["fingerprint"]
    check(other_fingerprint != previous_fingerprint and other_fingerprint != imported_fingerprint,
          "the other ssh session did not replace the platform key")
    stored = page.request.get(base + key_path)
    check(stored.status == 200 and stored.json()["fingerprint"] == other_fingerprint,
          "the other ssh session did not win")
    check(field.input_value() == private_pem, "the open import draft changed")
    expect(modal).to_be_visible()
    expect(card.get_by_text(previous_fingerprint, exact=True)).to_be_visible()
    check(modal.locator(".ant-alert-error").count() == 0, "the import dialog shows a conflict")
    return {
        "other_session": 200,
        "previous_fingerprint": previous_fingerprint,
        "other_fingerprint": other_fingerprint,
        "draft_unchanged": True,
        "conflict_status": False,
    }


def exercise_ssh_rotate_minimum(page, card, ssh, initial, nodes_tab_name, base, key_path, rotate_path):
    """Rotate through the UI with no fields. The button, POST, card, reload and GET agree."""
    rotate = button(card, ssh["rotate"])
    expect(card.locator("button.ant-btn-dangerous").filter(has_text=ssh["rotate"])).to_have_count(1)
    seen = []

    def on_request(request):
        if request.method == "POST" and urlsplit(request.url).path == rotate_path:
            seen.append(request)

    page.on("request", on_request)
    rotate.click()
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_be_visible(timeout=5000)
    expect(popconfirm).to_contain_text(ssh["rotateConfirm"])
    expect(popconfirm).to_contain_text(ssh["rotateWarning"])
    with page.expect_response(lambda response: urlsplit(response.url).path == rotate_path
                              and response.request.method == "POST") as posted:
        popconfirm.locator(".ant-btn-primary").click()
    page.remove_listener("request", on_request)
    check(posted.value.status == 200, f"rotate HTTP {posted.value.status}")
    check(len(seen) == 1 and not seen[0].post_data, "rotate did not send one empty POST")
    body = posted.value.json()
    check(not any("private" in key.lower() for key in body), "the rotate response exposes a private field")
    expect(page.get_by_text(ssh["rotated"], exact=True).first).to_be_visible()
    stored_response = page.request.get(base + key_path)
    check(stored_response.status == 200, f"rotated GET HTTP {stored_response.status}")
    stored = stored_response.json()
    check(stored["fingerprint"] == body["fingerprint"] != initial["fingerprint"],
          "rotate response and GET do not agree on a new fingerprint")
    check(stored["public_key"] == body["public_key"] != initial["public_key"],
          "rotate response and GET do not agree on a new public key")
    check(stored["authorize_command"] == body["authorize_command"] != initial["authorize_command"],
          "rotate response and GET do not agree on a new authorize command")
    page.reload()
    expect(page.get_by_role("tab", name=nodes_tab_name, exact=True)).to_have_attribute("aria-selected", "true")
    refreshed = page.locator(".ant-card").filter(has_text=ssh["title"]).first
    expect(refreshed.get_by_text(stored["fingerprint"], exact=True)).to_be_visible()
    expect(refreshed.get_by_text(stored["authorize_command"], exact=True)).to_be_visible()
    return stored, {
        "fields_filled": 0,
        "dangerous_button": True,
        "method": "POST",
        "path": rotate_path,
        "body_empty": True,
        "status": 200,
        "response_matches_get": True,
        "reload_matches": True,
    }
