"""First-open inventory of the login form controls."""
from __future__ import annotations

import re

from platform_token_read_revoke import check
from playwright.sync_api import expect

USERNAME_AUTOCOMPLETE = "username"
PASSWORD_AUTOCOMPLETE = "current-password"


def exercise_login_inventory(page, auth):
    """Labels, autocomplete, empty defaults, and controls the login form does not render."""
    username = page.get_by_role("textbox", name=auth["username"], exact=True)
    secret = page.get_by_label(auth["password"], exact=True)
    expect(username).to_have_attribute("autocomplete", USERNAME_AUTOCOMPLETE)
    expect(secret).to_have_attribute("autocomplete", PASSWORD_AUTOCOMPLETE)
    expect(username).to_be_visible()
    expect(secret).to_be_visible()
    expect(username).to_have_class(re.compile(r"\bant-input-lg\b"))
    expect(secret).to_have_class(re.compile(r"\bant-input-lg\b"))
    expect(username).to_be_empty()
    expect(secret).to_be_empty()
    expect(secret).to_have_attribute("type", "password")
    check(username.get_attribute("maxlength") is None, "the username has a maximum length")
    check(secret.get_attribute("maxlength") is None, "the password has a maximum length")
    expect(page.get_by_role("spinbutton")).to_have_count(0)
    return {
        "username_autocomplete": USERNAME_AUTOCOMPLETE,
        "password_autocomplete": PASSWORD_AUTOCOMPLETE,
        "username_empty": True,
        "password_empty": True,
        "password_type": "password",
        "large_inputs": 2,
        "spinbuttons": 0,
    }
