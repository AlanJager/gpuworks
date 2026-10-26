"""Keyboard, error, and layout checks for the deployment start-timeout card."""
from __future__ import annotations

from playwright.sync_api import expect
from platform_token_read_revoke import check


def _box(locator):
    return locator.evaluate("""element => {
        const box = element.getBoundingClientRect();
        return {left: box.left, top: box.top, right: box.right, bottom: box.bottom,
                width: box.width, height: box.height};
    }""")


def _separated(first, second):
    return (first["right"] <= second["left"] or second["right"] <= first["left"]
            or first["bottom"] <= second["top"] or second["bottom"] <= first["top"])


def exercise_timeout_keyboard(page, card, field, save, puts, original):
    """Tab reaches the spinbutton, Enter does not save, an empty value disables Save."""
    card.scroll_into_view_if_needed()
    expect(field).to_be_in_viewport(ratio=1)
    expect(save).to_be_in_viewport(ratio=1)
    field_box, save_box = _box(field), _box(save)
    check(field_box["width"] > 0 and save_box["width"] > 0, "timeout field or Save has no box")
    check(_separated(field_box, save_box), "timeout field overlaps Save")
    before = len(puts)
    field.press("Enter")
    check(len(puts) == before, "Enter in the timeout submitted a deployment policy")
    expect(field).to_have_value(str(original))
    field.focus()
    expect(field).to_be_focused()
    page.keyboard.press("Shift+Tab")
    expect(field).not_to_be_focused()
    page.keyboard.press("Tab")
    expect(field).to_be_focused()
    field.fill("")
    expect(field).to_have_value("")
    expect(save).to_be_disabled()
    check(len(puts) == before, "an empty timeout submitted a deployment policy")
    extra = card.locator(".ant-form-item-extra")
    expect(extra).to_be_visible()
    expect(extra).to_contain_text("60")
    expect(extra).to_contain_text("86400")
    field.fill(str(original))
    field.blur()
    expect(field).to_have_value(str(original))
    expect(save).to_be_enabled()
    return {"tab_returns": True, "enter_submits": False, "empty_disables_save": True,
            "range_help_visible": True, "viewport": "1440x900", "overlaps_save": False}
