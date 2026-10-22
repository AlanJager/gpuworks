"""Browser and resource-lifetime helpers for external benchmark acceptance.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import json
import re
from playwright.sync_api import expect
from local_browser_fixture import ROOT


def strings(locale):
    ui = json.loads((ROOT / "ui/src/i18n/locales" / locale / "evaluations.json").read_text())
    dictionary = json.loads((ROOT / "pkg/bench/locales" / f"{locale}.json").read_text())
    return ui, dictionary


def format_text(template, **values):
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", str(value))
    return template


def action(scope, label):
    accessible = r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label) else re.escape(label)
    return scope.get_by_role("button", name=re.compile(accessible)).first


def confirm_popconfirm(page):
    popconfirm = page.locator(".ant-popconfirm")
    expect(popconfirm).to_be_visible(timeout=5000)
    popconfirm.locator(".ant-btn-primary").click()


def assert_point_table(page, ui, point):
    table = page.locator(".ant-table-wrapper").filter(
        has=page.get_by_text(ui["pointTable"]["title"], exact=True))
    expect(table).to_have_count(1)
    row = table.locator("tbody tr.ant-table-row").filter(
        has=page.get_by_role("cell", name=f"#{point['seq']}", exact=True))
    expect(row).to_have_count(1)
    headers = table.locator("thead").first.locator("th").all_text_contents()
    cells = row.get_by_role("cell")
    index = headers.index("ISL / OSL")
    expect(cells.nth(index)).to_have_text(f"{point['actual_isl']:g} / {point['actual_osl']:g}")
    for key, field, unit in (("avg_output", "output_tput", "tok/s"),
                             ("avg_total", "total_tput", "tok/s"),
                             ("request", "request_tput", "req/s"),
                             ("ttft_p99", "p99_ttft", "ms"),
                             ("tpot_p99", "p99_tpot", "ms"),
                             ("latency_p99", "p99_latency", "ms"),
                             ("interactivity", "interactivity", "tok/s/user")):
        title = ui["pointTable"]["column"][key]
        index = next(i for i, header in enumerate(headers) if title in header)
        text = cells.nth(index).inner_text().strip()
        expected = (1000 / point["p50_tpot"] if point["p50_tpot"] > 0 else None) if field == "interactivity" else point[field]
        if expected is None or (field == "p99_latency" and expected <= 0):
            assert text == "—", f"UI {key} should show unavailable"
            continue
        assert text.endswith(" " + unit), f"UI {key} unit differs: {text}"
        value = float(text[:-(len(unit) + 1)].replace(",", ""))
        assert abs(value - expected) <= 0.051, f"UI {key} differs: {text}"


def close_browser(context, browser, report, artifacts):
    errors = []
    for resource in (context, browser):
        try:
            resource.close()
        except Exception as error:
            errors.append(artifacts.redact(f"{type(error).__name__}: {error}"))
    report["browser_close_ok"] = not errors
    if errors:
        report["browser_close_errors"] = errors
        report["status"] = "FAIL_TEST"


def finish_environment(server, fixture, report):
    errors = []
    try:
        report["server_stopped"] = server.stop()
    except Exception as error:
        report["server_stopped"] = False
        errors.append(f"server stop: {type(error).__name__}")
    try:
        report["fixture_stopped"] = fixture is None or fixture.stop()
    except Exception as error:
        report["fixture_stopped"] = False
        errors.append(f"fixture stop: {type(error).__name__}")
    if errors:
        report["cleanup_errors"] = errors
    report["fixture_active_handlers"] = fixture.active_handler_count if fixture is not None else 0
    report["environment_cleanup_ok"] = bool(report["server_stopped"]
                                             and report["fixture_stopped"]
                                             and report.get("browser_close_ok", True))
    if not report["environment_cleanup_ok"]:
        report["status"] = "FAIL_TEST"
