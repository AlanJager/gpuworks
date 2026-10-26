"""Saved-label edits for the node labels dialog.

One value change, one clear, one rebind, and one removal. The dialog has no
switch. Each save changes a single label fact, and the independent node read
matches that save. The node name and server url stay.
"""
import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import check

NODES = "/api/v1/nodes"
_ALIGN = re.compile(r"\bant-space-align-baseline\b")


def _add(scope, label):
    accessible = r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile(accessible)).first


def _index(keys, wanted):
    for index in range(keys.count()):
        if keys.nth(index).input_value() == wanted:
            return index
    check(False, f"the {wanted} label was not backfilled")
    return 0


def exercise_label_edit_changes(page, base, node, ui, open_labels):
    """A→B, empty, rebind, and remove, with the other label facts left in place."""
    labels_path = f"{NODES}/{node['uuid']}/labels"
    node_path = f"{NODES}/{node['uuid']}"
    puts = []

    def read_node():
        response = page.request.get(base + node_path)
        check(response.status == 200, f"label edit GET HTTP {response.status}")
        return response.json()

    def same_identity(before, after):
        for field in ("uuid", "name", "server_url", "state", "scheduling"):
            check(after.get(field) == before.get(field),
                  f"label edit changed {field}: {before.get(field)!r} -> {after.get(field)!r}")

    def save(dialog, body):
        with page.expect_response(lambda response: urlsplit(response.url).path == labels_path
                                  and response.request.method == "PUT") as saved:
            dialog.locator(".ant-modal-footer .ant-btn-primary").click()
        check(saved.value.status == 200, f"label edit HTTP {saved.value.status} for {body}")
        check(saved.value.request.post_data_json == {"labels": body},
              f"label edit body {saved.value.request.post_data_json}")
        expect(dialog).not_to_be_visible()
        puts.append({"method": "PUT", "path": labels_path, "body": {"labels": body}})
        after = read_node()
        check(after["labels"] == body, f"label edit readback {after.get('labels')!r}")
        same_identity(origin, after)
        return after

    origin = read_node()
    check(origin["labels"] == {"team": "alpha"}, f"label edit start {origin.get('labels')!r}")
    dialog = open_labels()
    expect(dialog.get_by_role("switch")).to_have_count(0)
    spaces = dialog.locator(".ant-space")
    expect(spaces).to_have_count(1)
    expect(spaces).to_have_class(_ALIGN)
    key = dialog.get_by_label(ui["labels"]["key"], exact=True)
    value = dialog.get_by_label(ui["labels"]["value"], exact=True)
    expect(key).to_have_value("team")
    expect(value).to_have_value("alpha")
    value.fill("beta")
    expect(key).to_have_value("team")
    save(dialog, {"team": "beta"})
    row = page.get_by_role("row").filter(has_text=node["name"])
    expect(row.get_by_text("team=beta", exact=True)).to_be_visible()
    expect(row.get_by_text(node["name"], exact=True)).to_be_visible()

    dialog = open_labels()
    key = dialog.get_by_label(ui["labels"]["key"], exact=True)
    value = dialog.get_by_label(ui["labels"]["value"], exact=True)
    expect(key).to_have_value("team")
    expect(value).to_have_value("beta")
    value.fill("")
    expect(key).to_have_value("team")
    save(dialog, {"team": ""})
    row = page.get_by_role("row").filter(has_text=node["name"])
    expect(row.get_by_text("team=", exact=True)).to_be_visible()

    dialog = open_labels()
    key = dialog.get_by_label(ui["labels"]["key"], exact=True)
    value = dialog.get_by_label(ui["labels"]["value"], exact=True)
    expect(key).to_have_value("team")
    expect(value).to_have_value("")
    value.fill("alpha")
    expect(key).to_have_value("team")
    save(dialog, {"team": "alpha"})

    dialog = open_labels()
    _add(dialog, ui["labels"]["add"]).click()
    keys = dialog.get_by_label(ui["labels"]["key"], exact=True)
    values = dialog.get_by_label(ui["labels"]["value"], exact=True)
    expect(keys).to_have_count(2)
    expect(dialog.locator(".ant-space")).to_have_count(2)
    expect(dialog.locator(".ant-space").nth(0)).to_have_class(_ALIGN)
    expect(dialog.locator(".ant-space").nth(1)).to_have_class(_ALIGN)
    expect(keys.nth(0)).to_have_value("team")
    expect(values.nth(0)).to_have_value("alpha")
    keys.nth(1).fill("site")
    values.nth(1).fill("east")
    expect(keys.nth(0)).to_have_value("team")
    expect(values.nth(0)).to_have_value("alpha")
    save(dialog, {"team": "alpha", "site": "east"})
    row = page.get_by_role("row").filter(has_text=node["name"])
    expect(row.get_by_text("team=alpha", exact=True)).to_be_visible()
    expect(row.get_by_text("site=east", exact=True)).to_be_visible()

    dialog = open_labels()
    keys = dialog.get_by_label(ui["labels"]["key"], exact=True)
    values = dialog.get_by_label(ui["labels"]["value"], exact=True)
    expect(keys).to_have_count(2)
    site_at = _index(keys, "site")
    team_at = 1 - site_at
    expect(keys.nth(team_at)).to_have_value("team")
    expect(values.nth(team_at)).to_have_value("alpha")
    expect(values.nth(site_at)).to_have_value("east")
    keys.nth(site_at).fill("zone")
    expect(keys.nth(team_at)).to_have_value("team")
    expect(values.nth(team_at)).to_have_value("alpha")
    expect(values.nth(site_at)).to_have_value("east")
    save(dialog, {"team": "alpha", "zone": "east"})

    dialog = open_labels()
    keys = dialog.get_by_label(ui["labels"]["key"], exact=True)
    values = dialog.get_by_label(ui["labels"]["value"], exact=True)
    zone_at = _index(keys, "zone")
    team_at = 1 - zone_at
    expect(keys.nth(team_at)).to_have_value("team")
    expect(values.nth(team_at)).to_have_value("alpha")
    expect(values.nth(zone_at)).to_have_value("east")
    dialog.get_by_role("button", name=ui["labels"]["remove"], exact=True).nth(zone_at).click()
    expect(dialog.get_by_label(ui["labels"]["key"], exact=True)).to_have_count(1)
    expect(dialog.get_by_label(ui["labels"]["key"], exact=True)).to_have_value("team")
    expect(dialog.get_by_label(ui["labels"]["value"], exact=True)).to_have_value("alpha")
    save(dialog, {"team": "alpha"})
    row = page.get_by_role("row").filter(has_text=node["name"])
    expect(row.get_by_text("team=alpha", exact=True)).to_be_visible()
    expect(row.get_by_text("zone=east", exact=True)).to_have_count(0)
    return {
        "switch": False,
        "row_align": "baseline",
        "value_to_beta": {"team": "beta"},
        "value_to_empty": {"team": ""},
        "value_restored": {"team": "alpha"},
        "binding_added": {"team": "alpha", "site": "east"},
        "binding_changed": {"team": "alpha", "zone": "east"},
        "binding_removed": {"team": "alpha"},
        "name": origin["name"],
        "server_url": origin.get("server_url", ""),
        "puts": puts,
    }
