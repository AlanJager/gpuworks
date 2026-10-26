"""First-open inventory of the network create dialog."""
from __future__ import annotations

from platform_token_read_revoke import check
from playwright.sync_api import expect

TYPES = ["host_ethernet", "host_rdma", "accelerator_rdma"]
ROLES = ["storage_sync", "compute", "serving"]
CIDR_PLACEHOLDER = "10.20.0.0/16"


def exercise_network_create_inventory(dialog, t):
    """Labels, control types, required flags, defaults, range, and ethernet condition."""
    expect(dialog.locator(".ant-radio-button-wrapper")).to_have_count(3)
    for kind in TYPES:
        wrapper = dialog.locator(".ant-radio-button-wrapper").filter(has_text=t["type"][kind])
        expect(wrapper).to_have_count(1)
        expect(wrapper).to_be_visible()
        radio = dialog.get_by_role("radio", name=t["type"][kind], exact=True)
        expect(radio).to_be_checked(checked=kind == TYPES[0])
    expect(dialog.locator(".ant-radio-group")).to_have_count(1)
    check(dialog.locator(".ant-radio-group").get_attribute("aria-required") != "true", "the type is required")

    name = dialog.get_by_label(t["form"]["name"], exact=True)
    expect(name).to_be_visible()
    expect(name).to_be_empty()
    expect(name).to_have_attribute("type", "text")
    expect(name).to_have_attribute("aria-required", "true")
    expect(name).to_have_attribute("maxlength", "64")
    check(name.get_attribute("placeholder") is None, "the name has a placeholder")

    description = dialog.get_by_label(t["form"]["description"], exact=True)
    expect(description).to_be_visible()
    expect(description).to_be_empty()
    expect(description).to_have_attribute("type", "text")
    check(description.get_attribute("aria-required") != "true", "the description is required")
    expect(description).to_have_attribute("maxlength", "256")
    check(description.get_attribute("placeholder") is None, "the description has a placeholder")

    expect(dialog.locator(".ant-checkbox-group")).to_have_count(1)
    expect(dialog.locator(".ant-checkbox-group")).to_have_attribute("aria-required", "true")
    expect(dialog.get_by_role("checkbox")).to_have_count(3)
    for role in ROLES:
        box = dialog.get_by_role("checkbox", name=t["role"][role], exact=True)
        expect(box).to_be_visible()
        expect(box).to_be_enabled()
        expect(box).not_to_be_checked()

    cidr = dialog.get_by_label(t["form"]["cidr"], exact=True)
    expect(cidr).to_be_visible()
    expect(cidr).to_be_empty()
    expect(cidr).to_have_attribute("type", "text")
    expect(cidr).to_have_attribute("placeholder", CIDR_PLACEHOLDER)
    check(cidr.get_attribute("aria-required") != "true", "the cidr is required")
    check(cidr.get_attribute("maxlength") is None, "the cidr has a maximum length")
    interface = dialog.get_by_label(t["form"]["interface"], exact=True)
    expect(interface).to_be_visible()
    expect(interface).to_be_empty()
    expect(interface).to_have_attribute("type", "text")
    expect(interface).to_have_attribute("placeholder", t["form"]["interfacePlaceholder"])
    check(interface.get_attribute("aria-required") != "true", "the interface is required")
    check(interface.get_attribute("maxlength") is None, "the interface has a maximum length")
    expect(dialog.get_by_label(t["form"]["bootstrapCidr"], exact=True)).to_have_count(0)
    expect(dialog.get_by_role("button").filter(has_text=t["form"]["advanced"])).to_have_count(0)

    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.locator(".ant-input-group-addon")).to_have_count(0)
    expect(dialog.get_by_role("combobox")).to_have_count(0)
    expect(dialog.get_by_role("textbox")).to_have_count(4)
    return {
        "button_radios": 3,
        "default_type": TYPES[0],
        "name_maxlength": 64,
        "description_maxlength": 256,
        "name_required": True,
        "roles_required": True,
        "description_required": False,
        "type_required": False,
        "cidr_placeholder": CIDR_PLACEHOLDER,
        "cidr_visible": True,
        "interface_visible": True,
        "bootstrap_visible": False,
        "spinbuttons": 0,
        "units": 0,
        "textboxes": 4,
    }
