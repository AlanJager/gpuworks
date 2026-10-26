"""First-open inventory of the local storage add dialog."""
from __future__ import annotations

import re

from platform_token_read_revoke import button, check
from playwright.sync_api import expect

TYPES = ["local", "nfs", "nfs_cache", "shared_dir"]
SCOPES = ["all", "labels", "nodes"]


def _exact(value):
    return re.compile("^" + re.escape(value) + "$")


def exercise_storage_create_inventory(dialog, t, type_labels):
    """Labels, control types, defaults, range text, and the local-only condition."""
    note = t["form"]["typeHelp"]["local"]
    help_text = dialog.locator("div.ant-typography-secondary").filter(has_text=note)
    expect(help_text).to_have_count(1)
    expect(help_text).to_be_visible()
    expect(help_text).to_have_text(note)
    expect(help_text).to_have_class(re.compile(r"\bant-typography-secondary\b"))

    expect(dialog.locator(".ant-radio-button-wrapper")).to_have_count(7)
    expect(dialog.locator(".ant-radio-group")).to_have_count(2)
    type_group = dialog.locator(f'.ant-radio-group[aria-label="{t["form"]["type"]}"]')
    expect(type_group).to_have_count(1)
    check(type_group.get_attribute("aria-required") != "true", "the type is required")
    expect(type_group.locator(".ant-radio-button-wrapper")).to_have_count(4)
    for kind in TYPES:
        label = type_labels[kind]
        wrapper = type_group.locator(".ant-radio-button-wrapper").filter(has_text=_exact(label))
        expect(wrapper).to_have_count(1)
        expect(wrapper).to_be_visible()
        radio = type_group.get_by_role("radio", name=label, exact=True)
        expect(radio).to_be_checked(checked=kind == "local")

    name = dialog.get_by_label(t["form"]["name"], exact=True)
    expect(name).to_be_visible()
    expect(name).to_be_empty()
    expect(name).to_have_attribute("type", "text")
    check(name.get_attribute("aria-required") != "true", "the name is marked required")
    check(name.get_attribute("maxlength") is None, "the name has a maximum length")
    check(name.get_attribute("placeholder") is None, "the name has a placeholder")
    expect(dialog.get_by_text(t["form"]["nameHelp"], exact=True)).to_be_visible()

    directory = dialog.get_by_label(t["form"]["path"], exact=True)
    expect(directory).to_be_visible()
    expect(directory).to_be_empty()
    expect(directory).to_have_attribute("type", "text")
    check(directory.get_attribute("aria-required") != "true", "the directory is marked required")
    check(directory.get_attribute("maxlength") is None, "the directory has a maximum length")
    expect(dialog.get_by_text(t["form"]["pathHelp"], exact=True)).to_be_visible()

    for absent in ("export", "mountOptions", "cachePath", "sharedPath", "mountPoint"):
        expect(dialog.get_by_label(t["form"][absent], exact=True)).to_have_count(0)
    expect(dialog.locator("#storage-export")).to_have_count(0)
    expect(dialog.locator("#storage-options")).to_have_count(0)
    expect(dialog.locator("#storage-cache-path")).to_have_count(0)
    expect(dialog.locator("#storage-cache-max")).to_have_count(0)
    expect(dialog.locator(".ant-steps")).to_have_count(0)
    expect(button(dialog, t["form"]["probe"])).to_have_count(0)
    expect(dialog.get_by_role("checkbox")).to_have_count(0)
    expect(dialog.get_by_role("textbox", name=t["form"]["scope"]["labelKey"], exact=True)).to_have_count(0)
    expect(dialog.get_by_role("alert")).to_have_count(0)

    scope_group = dialog.locator(f'.ant-radio-group[aria-label="{t["form"]["scope"]["title"]}"]')
    expect(scope_group).to_have_count(1)
    check(scope_group.get_attribute("aria-required") != "true", "the scope is required")
    expect(scope_group.locator(".ant-radio-button-wrapper")).to_have_count(3)
    for key in SCOPES:
        label = t["form"]["scope"][key]
        wrapper = scope_group.locator(".ant-radio-button-wrapper").filter(has_text=_exact(label))
        expect(wrapper).to_have_count(1)
        expect(wrapper).to_be_visible()
        radio = scope_group.get_by_role("radio", name=label, exact=True)
        expect(radio).to_be_checked(checked=key == "all")
    scope_help = dialog.locator("span.ant-typography-secondary").filter(has_text=t["form"]["scope"]["allHelp"])
    expect(scope_help).to_have_count(1)
    expect(scope_help).to_be_visible()
    expect(scope_help).to_have_text(t["form"]["scope"]["allHelp"])

    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.locator(".ant-input-group-addon")).to_have_count(0)
    combo = dialog.get_by_role("combobox", name=t["form"]["scope"]["overrideNodes"], exact=True)
    expect(combo).to_have_count(1)
    expect(combo).to_be_visible()
    expect(dialog.locator(".ant-select-selection-item")).to_have_count(0)
    return {
        "type_help": note,
        "button_radios": 7,
        "default_type": "local",
        "default_scope": "all",
        "name_required": False,
        "directory_required": False,
        "spinbuttons": 0,
        "units": 0,
        "comboboxes": 1,
        "hidden_export": True,
        "hidden_cache": True,
    }
