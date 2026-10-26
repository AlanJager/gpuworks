"""First-open inventory of the custom image drawer used by tagged registration."""
from __future__ import annotations

from image_catalog import button
from platform_token_read_revoke import check
from playwright.sync_api import expect

ARCHIVE_PLACEHOLDER = "https://containerimages.example/runner.tar"
REFERENCE_PLACEHOLDER = "registry.example/team/image:v1"
ARCHIVE_NAME = "image_archive_url"


def exercise_register_inventory(page, dialog, form, mutations):
    """Labels, types, defaults, and the archive control's name before any edit."""
    url = form["url"]
    address = dialog.get_by_role("textbox", name=url["label"], exact=True)
    expect(address).to_have_attribute("name", ARCHIVE_NAME)
    expect(address).to_have_attribute("type", "text")
    expect(address).to_have_value("")
    expect(address).to_have_attribute("placeholder", ARCHIVE_PLACEHOLDER)
    check(address.get_attribute("maxlength") is None, "the archive URL has a maximum length")
    check(address.get_attribute("aria-required") != "true", "the archive URL is required")
    expect(button(dialog, url["resolve"])).to_be_visible()
    expect(dialog.get_by_text(url["size"], exact=True)).to_have_count(0)
    expect(dialog.get_by_text(url["tags"], exact=True)).to_have_count(0)
    expect(dialog.get_by_text(form["community"]["title"], exact=True)).to_have_count(0)

    series = dialog.get_by_label(form["seriesName"], exact=True)
    reference = dialog.get_by_label(form["imageRef"], exact=True)
    engine_version = dialog.get_by_label(form["engineVersion"], exact=True)
    framework_version = dialog.get_by_label(form["frameworkVersion"], exact=True)
    expect(series).to_have_value("")
    check(series.get_attribute("aria-required") != "true", "the series name is required")
    check(series.get_attribute("maxlength") is None, "the series name has a maximum length")
    expect(series).to_have_attribute("placeholder", form["seriesNamePlaceholder"])
    expect(reference).to_have_value("")
    expect(reference).to_have_attribute("aria-required", "true")
    expect(reference).to_have_attribute("placeholder", REFERENCE_PLACEHOLDER)
    check(reference.get_attribute("maxlength") is None, "the image reference has a maximum length")
    expect(engine_version).to_have_value("")
    check(engine_version.get_attribute("aria-required") != "true", "the engine version is required")
    expect(engine_version).to_have_attribute("placeholder", form["exampleEngineVersion"])
    expect(framework_version).to_have_value("")
    expect(framework_version).to_have_attribute("aria-required", "true")
    expect(framework_version).to_have_attribute("placeholder", form["exampleFrameworkVersion"])
    expect(dialog.get_by_label(form["engine"], exact=True)).to_be_visible()
    expect(dialog.get_by_label(form["framework"], exact=True)).to_be_visible()
    expect(dialog.get_by_role("combobox", name=form["hostArch"], exact=True)).to_have_attribute("aria-expanded", "false")
    expect(dialog.locator(".ant-select-selection-item")).to_have_count(1)
    expect(dialog.locator(".ant-select-selection-item")).to_have_text("multi")
    switch = dialog.get_by_role("switch", name=form["rollingTag"], exact=True)
    expect(switch).to_be_visible()
    expect(switch).not_to_be_checked()
    check(switch.get_attribute("aria-required") != "true", "the rolling switch is required")
    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.get_by_role("textbox")).to_have_count(5)
    check(mutations == [], "reading the drawer sent an image mutation")
    return {
        "archive_name": ARCHIVE_NAME, "archive_type": "text", "archive_empty": True,
        "archive_placeholder": ARCHIVE_PLACEHOLDER, "reference_required": True,
        "reference_placeholder": REFERENCE_PLACEHOLDER, "arch": "multi",
        "rolling_checked": False, "spinbuttons": 0, "textboxes": 5, "mutations": 0,
    }
