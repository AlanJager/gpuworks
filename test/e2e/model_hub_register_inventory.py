"""First-open inventory of the hub metadata register dialog."""
from __future__ import annotations

import re

from platform_token_read_revoke import check
from playwright.sync_api import expect

SOURCE_ID_PLACEHOLDER = "Qwen/Qwen3-8B"


def exercise_hub_register_inventory(dialog, labels):
    """Labels, control types, required flags, and the collapsed ghost panel."""
    form = dialog.locator("form.ant-form-vertical")
    expect(form).to_have_count(1)
    expect(form).to_be_visible()
    panel = dialog.locator(".ant-collapse-ghost")
    expect(panel).to_have_count(1)
    expect(panel).to_be_visible()
    expect(panel).to_have_class(re.compile(r"\bant-collapse-ghost\b"))
    expect(panel).to_have_class(re.compile(r"\bant-collapse-small\b"))
    header = panel.locator(".ant-collapse-header")
    expect(header).to_have_count(1)
    expect(header).to_be_visible()
    expect(header).to_have_attribute("aria-expanded", "false")
    expect(header.locator(".ant-collapse-header-text")).to_have_text(labels["register"]["advanced"])
    expect(dialog.get_by_label(labels["detail"]["revision"], exact=True)).not_to_be_visible()

    source = dialog.get_by_label(labels["column"]["source"], exact=True)
    expect(source).to_be_visible()
    expect(source).to_have_attribute("aria-required", "true")
    expect(dialog.locator(".ant-select-selection-item")).to_have_count(0)
    expect(dialog.locator(".ant-select-selection-placeholder")).to_have_text(labels["register"]["chooseSource"])

    source_id = dialog.get_by_label(labels["column"]["sourceID"], exact=True)
    expect(source_id).to_be_visible()
    expect(source_id).to_be_empty()
    expect(source_id).to_have_attribute("type", "text")
    expect(source_id).to_have_attribute("aria-required", "true")
    expect(source_id).to_have_attribute("placeholder", SOURCE_ID_PLACEHOLDER)
    check(source_id.get_attribute("maxlength") is None, "the source ID has a maximum length")

    name = dialog.get_by_label(labels["column"]["name"], exact=True)
    expect(name).to_be_visible()
    expect(name).to_be_empty()
    expect(name).to_have_attribute("type", "text")
    check(name.get_attribute("aria-required") != "true", "the name is marked required")
    check(name.get_attribute("maxlength") is None, "the name has a maximum length")
    expect(name).to_have_attribute("placeholder", labels["register"]["displayNamePlaceholder"])

    expect(dialog.get_by_label(labels["register"]["directoryURL"], exact=True)).to_have_count(0)
    expect(dialog.get_by_label(labels["register"]["modelDirectory"], exact=True)).to_have_count(0)
    expect(dialog.get_by_label(labels["detail"]["node"], exact=True)).to_have_count(0)
    expect(dialog.get_by_label(labels["register"]["modelID"], exact=True)).to_have_count(0)
    expect(dialog.get_by_text(labels["register"]["parseDirectory"], exact=True)).to_have_count(0)
    expect(dialog.get_by_role("alert")).to_have_count(0)
    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.locator(".ant-input-group-addon")).to_have_count(0)
    expect(dialog.get_by_role("checkbox")).to_have_count(0)
    expect(dialog.get_by_role("combobox")).to_have_count(1)
    expect(dialog.get_by_role("textbox")).to_have_count(2)
    return {
        "vertical_form": True,
        "ghost_collapse": True,
        "collapse_small": True,
        "advanced_collapsed": True,
        "source_required": True,
        "source_selected": False,
        "source_id_required": True,
        "source_id_placeholder": SOURCE_ID_PLACEHOLDER,
        "name_required": False,
        "spinbuttons": 0,
        "units": 0,
        "checkboxes": 0,
        "alerts": 0,
        "comboboxes": 1,
        "textboxes": 2,
    }


def expect_hub_source_selected(dialog, labels, chosen):
    """The chosen repository stays on the same collapsed hub fields."""
    expect(dialog.locator(".ant-collapse-ghost")).to_have_count(1)
    expect(dialog.locator(".ant-collapse-header")).to_have_attribute("aria-expanded", "false")
    expect(dialog.get_by_label(labels["detail"]["revision"], exact=True)).not_to_be_visible()
    expect(dialog.locator(".ant-select-selection-item")).to_have_count(1)
    expect(dialog.locator(".ant-select-selection-item")).to_have_text(chosen)
    expect(dialog.get_by_label(labels["column"]["sourceID"], exact=True)).to_be_empty()
    expect(dialog.get_by_label(labels["column"]["name"], exact=True)).to_be_empty()
    return chosen
