"""First-open inventory of the add-AI-service dialog."""
from __future__ import annotations

from playwright.sync_api import expect
from platform_token_read_revoke import check


def exercise_add_inventory(modal, settings):
    """Labels, types, required flags, defaults, and the visible default switch."""
    ai = settings["ai"]
    expect(modal.locator(".ant-select-selection-item").first).to_have_text(ai["presetName"]["deepseek"])
    name = modal.get_by_label(ai["name"], exact=True)
    url = modal.get_by_label(ai["baseUrl"], exact=True)
    model = modal.get_by_label(ai["model"], exact=True)
    key = modal.get_by_label(ai["upstreamKey"], exact=True)
    expect(name).to_have_value("")
    expect(name).to_have_attribute("aria-required", "true")
    expect(name).to_have_attribute("placeholder", ai["namePlaceholder"])
    check(name.get_attribute("maxlength") is None, "the name has a maximum length")
    expect(url).to_have_value("https://api.deepseek.com")
    expect(url).to_have_attribute("aria-required", "true")
    check(url.get_attribute("maxlength") is None, "the base URL has a maximum length")
    expect(model).to_have_value("deepseek-v4-flash")
    expect(model).to_have_attribute("aria-required", "true")
    check(model.get_attribute("maxlength") is None, "the model has a maximum length")
    expect(key).to_have_attribute("type", "password")
    expect(key).to_have_value("")
    expect(key).to_have_attribute("aria-required", "true")
    expect(modal.get_by_role("textbox")).to_have_count(4)
    expect(modal.get_by_role("spinbutton")).to_have_count(0)
    switch = modal.get_by_role("switch")
    expect(switch).to_have_count(1)
    expect(switch).to_be_visible()
    expect(switch).to_be_checked()
    expect(modal.get_by_text(ai["setDefault"], exact=True)).to_be_visible()
    check(switch.get_attribute("aria-required") != "true", "the default switch is required")
    return {"preset": "deepseek", "name_empty": True, "base_url": "https://api.deepseek.com",
            "model": "deepseek-v4-flash", "key_empty": True, "key_type": "password",
            "switch_visible": True, "switch_checked": True, "switch_required": False,
            "textboxes": 4, "units": False}
