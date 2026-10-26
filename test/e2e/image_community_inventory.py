"""First-open inventory of the community image drawer."""
from __future__ import annotations

from image_catalog import button
from platform_token_read_revoke import check
from playwright.sync_api import expect

ARCHIVE_PLACEHOLDER = "https://containerimages.example/runner.tar"


def exercise_community_inventory(page, dialog, form, repos, imports):
    """Labels, types, defaults, and controls the community drawer shows before a browse."""
    community = form["community"]
    other = dialog.get_by_role("textbox", name=community["otherRepo"], exact=True)
    expect(other).to_have_attribute("type", "text")
    expect(other).to_have_value("")
    expect(other).to_be_visible()
    expect(other).to_have_attribute("placeholder", community["otherRepoPlaceholder"])
    check(other.get_attribute("aria-required") != "true", "the other repository is required")
    check(other.get_attribute("maxlength") is None, "the other repository has a maximum length")
    expect(button(dialog, community["browse"])).to_be_disabled()
    expect(dialog.get_by_role("textbox", name=community["search"], exact=True)).to_have_count(0)
    for repo in repos:
        expect(button(dialog, repo)).to_be_visible()

    archive = dialog.get_by_role("textbox", name=form["url"]["label"], exact=True)
    expect(archive).to_have_value("")
    expect(archive).to_have_attribute("placeholder", ARCHIVE_PLACEHOLDER)
    expect(dialog.get_by_text(form["url"]["size"], exact=True)).to_have_count(0)
    expect(dialog.get_by_text(form["url"]["tags"], exact=True)).to_have_count(0)

    series = dialog.get_by_label(form["seriesName"], exact=True)
    reference = dialog.get_by_label(form["imageRef"], exact=True)
    engine_version = dialog.get_by_label(form["engineVersion"], exact=True)
    framework_version = dialog.get_by_label(form["frameworkVersion"], exact=True)
    expect(series).to_have_value("")
    check(series.get_attribute("aria-required") != "true", "the series name is required")
    check(series.get_attribute("maxlength") is None, "the series name has a maximum length")
    expect(reference).to_have_value("")
    expect(reference).to_have_attribute("aria-required", "true")
    expect(engine_version).to_have_value("")
    check(engine_version.get_attribute("aria-required") != "true", "the engine version is required")
    expect(framework_version).to_have_value("")
    expect(framework_version).to_have_attribute("aria-required", "true")
    expect(dialog.locator(".ant-select-selection-item")).to_have_count(1)
    expect(dialog.locator(".ant-select-selection-item")).to_have_text("multi")
    switch = dialog.get_by_role("switch", name=form["rollingTag"], exact=True)
    expect(switch).to_be_visible()
    expect(switch).not_to_be_checked()
    check(switch.get_attribute("aria-required") != "true", "the rolling switch is required")
    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.get_by_role("textbox")).to_have_count(6)
    check(imports == [], "reading the drawer sent an image import")
    return {
        "other_type": "text", "other_empty": True, "other_required": False,
        "browse_disabled": True, "search_visible": False, "repos": list(repos),
        "arch": "multi",
        "rolling_checked": False, "spinbuttons": 0, "textboxes": 6, "imports": 0,
    }
