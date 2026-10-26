"""Field inventory of the HTTP-directory register dialog."""
from __future__ import annotations

from platform_token_read_revoke import button, check
from playwright.sync_api import expect

URL_PLACEHOLDER = "https://hf-mirror.example.com/Qwen/Qwen3-0.6B"


def exercise_http_register_inventory(dialog, k):
    """Labels, control types, required flags, defaults, and the URL-only extra."""
    note = k["register"]["directoryURLHelp"]
    extra = dialog.locator(".ant-form-item-extra").filter(has_text=note)
    expect(extra).to_have_count(1)
    expect(extra).to_be_visible()
    expect(extra).to_have_text(note)

    source = dialog.get_by_label(k["column"]["source"], exact=True)
    expect(source).to_be_visible()
    expect(source).to_have_attribute("aria-required", "true")
    expect(dialog.locator(".ant-select-selection-item")).to_have_count(1)
    expect(dialog.locator(".ant-select-selection-item")).to_have_text(k["source"]["url"])

    url = dialog.get_by_label(k["register"]["directoryURL"], exact=True)
    expect(url).to_be_visible()
    expect(url).to_be_empty()
    expect(url).to_have_attribute("type", "text")
    expect(url).to_have_attribute("aria-required", "true")
    expect(url).to_have_attribute("placeholder", URL_PLACEHOLDER)
    check(url.get_attribute("maxlength") is None, "the directory URL has a maximum length")

    name = dialog.get_by_label(k["column"]["name"], exact=True)
    expect(name).to_be_visible()
    expect(name).to_be_empty()
    expect(name).to_have_attribute("type", "text")
    check(name.get_attribute("aria-required") != "true", "the name is marked required")
    check(name.get_attribute("maxlength") is None, "the name has a maximum length")
    expect(name).to_have_attribute("placeholder", k["register"]["displayNamePlaceholder"])

    expect(button(dialog, k["register"]["parseDirectory"])).to_be_visible()
    expect(dialog.get_by_label(k["register"]["modelDirectory"], exact=True)).to_have_count(0)
    expect(dialog.get_by_label(k["detail"]["node"], exact=True)).to_have_count(0)
    expect(dialog.get_by_label(k["register"]["modelID"], exact=True)).to_have_count(0)
    expect(dialog.get_by_label(k["detail"]["revision"], exact=True)).to_have_count(0)
    expect(dialog.get_by_text(k["register"]["advanced"], exact=True)).to_have_count(0)
    expect(dialog.get_by_role("alert")).to_have_count(0)
    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.locator(".ant-input-group-addon")).to_have_count(0)
    expect(dialog.get_by_role("checkbox")).to_have_count(0)
    expect(dialog.get_by_role("combobox")).to_have_count(1)
    expect(dialog.get_by_role("textbox")).to_have_count(2)
    return {
        "form_extra": True,
        "default_source": "url",
        "url_required": True,
        "name_required": False,
        "url_placeholder": URL_PLACEHOLDER,
        "spinbuttons": 0,
        "units": 0,
        "comboboxes": 1,
        "textboxes": 2,
    }
