"""Name and path reject empty and whitespace. No storage create."""
from __future__ import annotations

import re
from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout, expect
from platform_token_read_revoke import button, check

STORAGES = "/api/v1/storages"


def _item(control):
    return control.locator(
        "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' ant-form-item ')][1]")


def exercise_storage_required(page, dialog, storage, directory):
    """Each required field rejects an empty value and a whitespace-only value."""
    name = dialog.get_by_label(storage["form"]["name"], exact=True)
    path = dialog.get_by_label(storage["form"]["path"], exact=True)
    save = button(dialog, storage["form"]["save"])
    name_error = storage["form"]["error"]["name"]
    path_error = storage["form"]["error"]["pathRequired"]
    name_item = _item(name)
    path_item = _item(path)

    def save_posts():
        try:
            with page.expect_request(
                    lambda request: request.method == "POST" and urlsplit(request.url).path == STORAGES,
                    timeout=1000):
                save.click()
            return True
        except PlaywrightTimeout:
            return False

    def located(item, message, control, other):
        expect(item).to_have_class(re.compile(r"ant-form-item-has-error"))
        expect(item.get_by_text(message, exact=True)).to_be_visible()
        expect(control).to_be_visible()
        expect(other).not_to_have_class(re.compile(r"ant-form-item-has-error"))

    path.fill(directory)
    name.fill("")
    check(not save_posts(), "empty name sent a storage create")
    expect(name).to_have_value("")
    located(name_item, name_error, name, path_item)

    name.fill("   ")
    check(not save_posts(), "whitespace name sent a storage create")
    expect(name).to_have_value("   ")
    located(name_item, name_error, name, path_item)

    name.fill("req-probe")
    path.fill("")
    check(not save_posts(), "empty path sent a storage create")
    expect(path).to_have_value("")
    located(path_item, path_error, path, name_item)

    path.fill("   ")
    check(not save_posts(), "whitespace path sent a storage create")
    expect(path).to_have_value("   ")
    located(path_item, path_error, path, name_item)

    return {"empty_name": True, "whitespace_name": True, "empty_path": True,
            "whitespace_path": True, "creates": 0}
