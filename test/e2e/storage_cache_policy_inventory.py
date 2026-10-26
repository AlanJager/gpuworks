"""First-open inventory and directory validation for the cache settings drawer."""
from __future__ import annotations

import re
from urllib.parse import urlsplit

from platform_token_read_revoke import button, check
from playwright.sync_api import expect


def exercise_node_policy_inventory(dialog, cm, cp):
    """Labels, control types, defaults, and the bordered usage list."""
    desc = dialog.locator(".ant-descriptions-bordered")
    expect(desc).to_have_count(1)
    expect(desc).to_be_visible()
    expect(desc).to_have_class(re.compile(r"\bant-descriptions-bordered\b"))
    expect(desc).to_have_class(re.compile(r"\bant-descriptions-small\b"))
    for label in (cm["used"], cm["protected"], cm["reclaimable"], cm["diskFree"]):
        expect(desc.locator("th.ant-descriptions-item-label").filter(has_text=label)).to_have_count(1)
    expect(desc.get_by_text(cm["unknown"], exact=True)).to_have_count(3)
    expect(desc.get_by_text(cm["unverified"], exact=True)).to_have_count(1)

    path_group = dialog.locator(f'.ant-radio-group[aria-label="{cm["pathMode"]}"]')
    expect(path_group).to_have_count(1)
    expect(path_group.get_by_role("radio", name=cm["keep"], exact=True)).to_be_checked()
    expect(path_group.get_by_role("radio", name=cm["followPath"], exact=True)).not_to_be_checked()
    expect(path_group.get_by_role("radio", name=cm["customPath"], exact=True)).not_to_be_checked()
    directory = dialog.get_by_label(cm["pathValue"], exact=True)
    expect(directory).to_be_disabled()
    expect(directory).to_have_attribute("type", "text")
    shown = directory.input_value()
    check(shown.startswith("/"), "the cache directory is not absolute")
    expect(dialog.get_by_text(cm["defaultPathValue"].replace("{{path}}", shown), exact=True)).to_be_visible()

    limit = dialog.get_by_role("radiogroup", name=cm["limit"], exact=True)
    expect(limit).to_have_count(1)
    expect(limit.get_by_role("radio", name=cp["unlimited"], exact=True)).to_be_checked()
    expect(limit.get_by_role("radio", name=cp["custom"], exact=True)).not_to_be_checked()
    expect(limit.get_by_role("radio", name=cp["inherit"], exact=True)).not_to_be_checked()
    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.get_by_role("combobox")).to_have_count(0)
    expect(dialog.locator(".ant-input-group-addon")).to_have_count(0)
    expect(dialog.get_by_role("checkbox")).to_have_count(0)
    expect(dialog.get_by_text(cm["budgetHelp"], exact=True)).to_be_visible()
    expect(dialog.get_by_text(cm["cachedModels"], exact=True)).to_be_visible()
    expect(dialog.get_by_text(cm["filesUnknown"], exact=True)).to_have_count(0)
    expect(dialog.locator("ul li")).to_have_count(0)
    expect(button(dialog, cm["save"])).to_be_disabled()
    return {
        "bordered_descriptions": True,
        "descriptions_small": True,
        "usage_labels": 4,
        "usage_unknown": 3,
        "disk_unverified": True,
        "path_mode": "keep",
        "directory_disabled": True,
        "directory_absolute": True,
        "limit_mode": "unlimited",
        "spinbuttons": 0,
        "comboboxes": 0,
        "units": 0,
        "checkboxes": 0,
        "save_disabled": True,
        "model_rows": 0,
    }


def exercise_node_path_required(page, dialog, cm, cp, persisted):
    """A custom directory rejects empty and whitespace. The radios keep a selection."""
    writes = []

    def on_request(request):
        if request.method == "PATCH" and urlsplit(request.url).path.endswith("/node-overrides"):
            writes.append(request.method)

    page.on("request", on_request)
    baseline = len(writes)
    before = persisted()
    before_path = before["cache_path"]
    check(before_path.startswith("/"), "the stored default directory is not absolute")
    path_group = dialog.locator(f'.ant-radio-group[aria-label="{cm["pathMode"]}"]')
    path_group.get_by_role("radio", name=cm["customPath"], exact=True).check()
    directory = dialog.get_by_label(cm["pathValue"], exact=True)
    expect(directory).to_be_enabled()
    shown = directory.input_value()
    check(shown.startswith("/"), "the custom directory did not start absolute")

    def reject(value):
        directory.fill(value)
        expect(directory).to_have_value(value)
        button(dialog, cm["save"]).click()
        expect(dialog.get_by_role("alert").filter(has_text=cm["invalidPath"])).to_be_visible()
        stored = persisted()
        node = next(item for item in stored["targets"] if item.get("node_uuid") == "cache-node-01")
        check(node["cache_max_bytes"] == 0 and "cache_path" not in node, "invalid directory changed the node target")
        check(stored["cache_path"] == before_path, "invalid directory changed the default directory")
        check(len(writes) == baseline, "invalid directory sent a node override")

    reject("")
    reject("   ")
    expect(path_group.get_by_role("radio", name=cm["customPath"], exact=True)).to_be_checked()
    expect(path_group.get_by_role("radio", name=cm["keep"], exact=True)).not_to_be_checked()
    expect(path_group.get_by_role("radio", name=cm["followPath"], exact=True)).not_to_be_checked()
    limit = dialog.get_by_role("radiogroup", name=cm["limit"], exact=True)
    expect(limit.get_by_role("radio", name=cp["unlimited"], exact=True)).to_be_checked()
    expect(limit.get_by_role("radio", name=cp["custom"], exact=True)).not_to_be_checked()
    expect(limit.get_by_role("radio", name=cp["inherit"], exact=True)).not_to_be_checked()
    path_group.get_by_role("radio", name=cm["keep"], exact=True).check()
    expect(directory).to_be_disabled()
    expect(button(dialog, cm["save"])).to_be_disabled()
    page.remove_listener("request", on_request)
    return {
        "custom_directory_enabled": True,
        "empty_rejected": True,
        "whitespace_rejected": True,
        "path_selection": "custom",
        "limit_selection": "unlimited",
        "patches": 0,
        "persisted_bytes": 0,
        "persisted_path_empty": True,
        "restored_keep": True,
    }
