"""First-open inventory of the RunPod account dialog."""
from __future__ import annotations

from platform_token_read_revoke import check
from playwright.sync_api import expect


def exercise_provider_create_inventory(dialog, copy, label, endpoint_default, endpoint_placeholder, key_placeholder):
    """Labels, types, required flags, defaults, and the API key visibility control."""
    key = dialog.get_by_label(label("api_key"), exact=True)
    expect(dialog.locator(".ant-input-password-icon")).to_have_count(1)
    expect(dialog.locator(".ant-input-password-icon")).to_be_visible()
    expect(key).to_have_attribute("type", "password")
    expect(key).to_be_empty()
    expect(key).to_have_attribute("placeholder", key_placeholder)
    expect(key).to_have_attribute("aria-required", "true")
    check(key.get_attribute("maxlength") is None, "the API key has a maximum length")
    name = dialog.get_by_label(copy["config"]["name"], exact=True)
    expect(name).to_be_visible()
    expect(name).to_be_empty()
    expect(name).to_have_attribute("aria-required", "true")
    expect(name).to_have_attribute("type", "text")
    check(name.get_attribute("maxlength") is None, "the name has a maximum length")
    expect(name).to_have_attribute(
        "placeholder", copy["config"]["namePlaceholder"].replace("{{kind}}", copy["kind"]["runpod"]))
    endpoint = dialog.get_by_label(label("endpoint"), exact=True)
    expect(endpoint).to_be_visible()
    expect(endpoint).to_have_value(endpoint_default)
    expect(endpoint).to_have_attribute("placeholder", endpoint_placeholder)
    expect(endpoint).to_have_attribute("type", "text")
    check(endpoint.get_attribute("aria-required") != "true", "the endpoint is required")
    check(endpoint.get_attribute("maxlength") is None, "the endpoint has a maximum length")
    expect(dialog.locator(".ant-select-selection-item")).to_have_text(copy["kind"]["runpod"])
    expect(dialog.get_by_role("combobox")).to_have_count(1)
    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.get_by_role("textbox")).to_have_count(3)
    return {
        "kind": copy["kind"]["runpod"],
        "name_empty": True,
        "name_required": True,
        "endpoint_default": endpoint_default,
        "endpoint_placeholder": endpoint_placeholder,
        "endpoint_required": False,
        "api_key_empty": True,
        "api_key_type": "password",
        "api_key_placeholder": key_placeholder,
        "api_key_required": True,
        "visibility_controls": 1,
        "spinbuttons": 0,
        "textboxes": 3,
    }
