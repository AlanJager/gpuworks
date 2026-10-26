"""Keyboard and layout of the platform SSH import dialog.

Enter in the private-key field does not import a key. An empty key leaves
Import disabled. Tab reaches Cancel and then Import, and at 1440×900 the
field and the footer actions stay on screen without covering each other.
"""
from __future__ import annotations

from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout
from playwright.sync_api import expect
from platform_token_read_revoke import button, check

KEY_PATH = "/api/v1/ssh-key"


def _box(locator):
    locator.scroll_into_view_if_needed()
    return locator.evaluate(
        """el => {
          const r = el.getBoundingClientRect();
          return {left: r.left, top: r.top, right: r.right, bottom: r.bottom,
                  width: r.width, height: r.height,
                  pageLeft: r.left + window.scrollX, pageTop: r.top + window.scrollY};
        }""")


def _overlaps(a, b):
    return not (a["pageLeft"] + a["width"] <= b["pageLeft"]
                or b["pageLeft"] + b["width"] <= a["pageLeft"]
                or a["pageTop"] + a["height"] <= b["pageTop"]
                or b["pageTop"] + b["height"] <= a["pageTop"])


def _enter_puts(page, field):
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path == KEY_PATH
                                  and request.method == "PUT", timeout=1000):
            field.press("Enter")
        return True
    except PlaywrightTimeout:
        return False


def exercise_ssh_keyboard(page, modal, ssh, ui):
    """Tab, Enter, the disabled Import, and the 1440×900 footer."""
    key = modal.get_by_label(ssh["privateKey"], exact=True)
    footer = modal.locator(".ant-modal-footer")
    import_key = button(footer, ssh["import"])
    cancel = button(footer, ui["common"]["cancel"])
    expect(key).to_have_value("")
    expect(import_key).to_be_disabled()
    key.focus()
    check(not _enter_puts(page, key), "Enter on an empty key imported an SSH key")
    expect(modal).to_be_visible()
    expect(key).to_have_value("\n")
    expect(import_key).to_be_disabled()
    key.fill("")
    key.focus()
    key.press("Tab")
    expect(cancel).to_be_focused()
    key.fill("ssh-probe")
    expect(import_key).to_be_enabled()
    key.focus()
    key.press("Tab")
    expect(cancel).to_be_focused()
    cancel.press("Tab")
    expect(import_key).to_be_focused()
    key.focus()
    check(not _enter_puts(page, key), "Enter submitted an SSH key")
    expect(modal).to_be_visible()
    expect(key).to_have_value("ssh-probe\n")
    expect(import_key).to_be_enabled()
    key.fill("")
    expect(import_key).to_be_disabled()
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    boxes = {label: _box(locator) for label, locator in (
        ("key", key), ("import", import_key), ("cancel", cancel))}
    for label, box in boxes.items():
        check(box["width"] > 0 and box["height"] > 0, f"{label} has no box")
        check(-1 <= box["left"] and box["right"] <= view["width"] + 1
              and -1 <= box["top"] and box["bottom"] <= view["height"] + 1,
              f"{label} is outside the viewport {box}")
    check(not _overlaps(boxes["key"], boxes["import"]), "the key overlaps Import")
    check(not _overlaps(boxes["key"], boxes["cancel"]), "the key overlaps Cancel")
    return {"tab": ["key", "cancel", "import"], "enter_submitted": False,
            "empty_key_disabled": True, "viewport": view}
