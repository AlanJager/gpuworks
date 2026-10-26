"""First-open inventory of the template weight-source dialog."""
from __future__ import annotations

import json

from platform_token_read_revoke import button, check
from playwright.sync_api import expect


def exercise_quick_weight_inventory(page, modal, deploy, repo):
    """Labels, the outlined source select, its options, and the disabled confirm."""
    expect(modal.get_by_text(deploy["action"]["selectWeightSourceDescription"], exact=True)).to_be_visible()
    select = modal.locator(".ant-select-outlined")
    expect(select).to_have_count(1)
    expect(select).to_be_visible()
    choose = modal.get_by_role("combobox", name=deploy["action"]["selectSourcePlaceholder"], exact=True)
    expect(choose).to_have_count(1)
    expect(choose).to_have_value("")
    check(choose.get_attribute("aria-required") in (None, "false"), "source combobox is marked required")
    check(choose.get_attribute("maxlength") is None, "source combobox has a maximum length")
    expect(modal.get_by_role("spinbutton")).to_have_count(0)
    expect(modal.get_by_role("checkbox")).to_have_count(0)
    expect(modal.get_by_role("radio")).to_have_count(0)
    expect(modal.locator(".ant-alert")).to_have_count(0)
    expect(button(modal, deploy["action"]["registerAndDeploy"])).to_be_disabled()
    choose.click()
    popup_id = choose.get_attribute("aria-controls")
    check(bool(popup_id), "source selector has no controlled popup")
    popup = page.locator("[id=" + json.dumps(popup_id) + "]").locator("..")
    options = [
        deploy["action"]["huggingFace"] + " · " + repo,
        deploy["action"]["modelScope"] + " · " + repo,
    ]
    for label in options:
        expect(popup.get_by_text(label, exact=True).filter(visible=True)).to_have_count(1)
    modal.locator(".ant-modal-title").click()
    expect(popup.get_by_text(options[0], exact=True).filter(visible=True)).to_have_count(0)
    return {
        "outlined_select": True,
        "description_visible": True,
        "comboboxes": 1,
        "combobox_empty": True,
        "combobox_required": False,
        "combobox_max_length": None,
        "spinbuttons": 0,
        "checkboxes": 0,
        "radios": 0,
        "alerts": 0,
        "confirm_disabled": True,
        "options": options,
    }
