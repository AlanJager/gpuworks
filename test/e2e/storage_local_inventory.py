"""First-open inventory of the local add-storage dialog."""
from __future__ import annotations

from playwright.sync_api import expect
from platform_token_read_revoke import check

def _item(control):
    return control.locator(
        "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")


def exercise_local_inventory(modal, storage, common, posts):
    """Labels, types, defaults, units, range help, and fields the local type hides."""
    form = storage["form"]
    types = common["storageType"]
    name = modal.get_by_label(form["name"], exact=True)
    path = modal.get_by_label(form["path"], exact=True)
    name_item = _item(name)
    path_item = _item(path)
    expect(name_item.get_by_text(form["nameHelp"], exact=True)).to_be_visible()
    expect(name).to_have_value("")
    check(name.get_attribute("maxlength") is None, "the name has a maximum length")
    check("GiB" not in name_item.inner_text(), "the name has a unit")
    expect(path).to_have_value("")
    expect(path_item.get_by_text(form["pathHelp"], exact=True)).to_be_visible()
    check(path.get_attribute("maxlength") is None, "the directory has a maximum length")
    check("GiB" not in path_item.inner_text(), "the directory has a unit")
    expect(modal.get_by_text(form["typeHelp"]["local"], exact=True)).to_be_visible()
    local = modal.get_by_role("radio", name=types["local"], exact=True)
    expect(local).to_be_checked()
    for key in ("nfs", "nfs_cache", "shared_dir"):
        option = modal.get_by_role("radio", name=types[key], exact=True)
        expect(option).to_have_count(1)
        expect(option).not_to_be_checked()
    expect(modal.get_by_label(form["export"], exact=True)).to_have_count(0)
    expect(modal.get_by_label(form["mountOptions"], exact=True)).to_have_count(0)
    expect(modal.get_by_label(form["cachePath"], exact=True)).to_have_count(0)
    scope = form["scope"]
    expect(modal.get_by_role("radio", name=scope["all"], exact=True)).to_be_checked()
    expect(modal.get_by_role("radio", name=scope["labels"], exact=True)).not_to_be_checked()
    expect(modal.get_by_role("radio", name=scope["nodes"], exact=True)).not_to_be_checked()
    expect(modal.get_by_role("radio", name=scope["mixed"], exact=True)).to_have_count(0)
    expect(modal.get_by_text(scope["allHelp"], exact=True)).to_be_visible()
    expect(modal.get_by_role("spinbutton")).to_have_count(0)
    expect(modal.get_by_role("textbox")).to_have_count(2)
    check(posts == [], "reading the add dialog sent a storage create")
    return {
        "type": "local", "name_empty": True, "name_help": True, "name_max_length": False,
        "path_empty": True, "path_help": True, "export_visible": False,
        "mount_options_visible": False, "cache_visible": False, "scope": "all",
        "spinbuttons": 0, "textboxes": 2, "posts": 0,
    }
