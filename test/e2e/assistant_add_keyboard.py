"""Keyboard and layout of the add-AI-service dialog.

Enter in the name field does not create a service. An empty name shows the
field error. Tab reaches the text fields, and at 1440×900 those fields and
the footer actions stay on screen without covering each other.
"""
from __future__ import annotations

from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout
from playwright.sync_api import expect
from platform_token_read_revoke import button, check

CONFIGS = "/api/v1/ai/configs"


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


def _enter_posts(page, field):
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path == CONFIGS
                                  and request.method == "POST", timeout=1000):
            field.press("Enter")
        return True
    except PlaywrightTimeout:
        return False


def exercise_add_keyboard(page, modal, settings, name_value):
    """Tab, Enter, the empty-name error, and the 1440×900 footer."""
    ai = settings["ai"]
    name = modal.get_by_label(ai["name"], exact=True)
    url = modal.get_by_label(ai["baseUrl"], exact=True)
    model = modal.get_by_label(ai["model"], exact=True)
    key = modal.get_by_label(ai["upstreamKey"], exact=True)
    footer = modal.locator(".ant-modal-footer")
    add = button(footer, settings["common"]["add"])
    cancel = button(footer, settings["common"]["cancel"])
    expect(name).to_have_value(name_value)
    name.fill("")
    check(not _enter_posts(page, name), "Enter on an empty name created an AI service")
    expect(modal).to_be_visible()
    expect(modal.get_by_text(ai["nameRequired"], exact=True)).to_be_visible()
    expect(name).to_have_attribute("aria-invalid", "true")
    expect(name).to_have_value("")
    name.fill(name_value)
    expect(name).to_have_value(name_value)
    name.focus()
    expect(name).to_be_focused()
    name.press("Tab")
    expect(url).to_be_focused()
    url.press("Tab")
    expect(model).to_be_focused()
    model.press("Tab")
    expect(key).to_be_focused()
    add.focus()
    expect(add).to_be_focused()
    name.focus()
    check(not _enter_posts(page, name), "Enter submitted an AI service")
    expect(modal).to_be_visible()
    expect(name).to_have_value(name_value)
    expect(url).to_have_value(url.input_value())
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    boxes = {label: _box(locator) for label, locator in (
        ("name", name), ("url", url), ("model", model), ("key", key),
        ("add", add), ("cancel", cancel))}
    for label, box in boxes.items():
        check(box["width"] > 0 and box["height"] > 0, f"{label} has no box")
        check(-1 <= box["left"] and box["right"] <= view["width"] + 1
              and -1 <= box["top"] and box["bottom"] <= view["height"] + 1,
              f"{label} is outside the viewport {box}")
    check(not _overlaps(boxes["name"], boxes["add"]), "the name overlaps Add")
    check(not _overlaps(boxes["key"], boxes["add"]), "the key overlaps Add")
    check(not _overlaps(boxes["name"], boxes["cancel"]), "the name overlaps Cancel")
    check(not _overlaps(boxes["key"], boxes["cancel"]), "the key overlaps Cancel")
    return {"tab": ["name", "base url", "model", "key", "add"],
            "enter_submitted": False, "empty_name_error": True, "viewport": view}
