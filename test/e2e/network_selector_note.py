"""Ethernet create dialog shows one info alert with the selector note."""
from __future__ import annotations

import re

from playwright.sync_api import expect


def exercise_selector_note(dialog, note):
    """The default ethernet form shows one info alert and its localized message."""
    alert = dialog.get_by_role("alert")
    expect(alert).to_have_count(1)
    expect(alert).to_be_visible()
    expect(alert).to_have_class(re.compile(r"\bant-alert-info\b"))
    expect(alert.locator(".ant-alert-message")).to_have_text(note)
    return {"alerts": 1, "info": True}
