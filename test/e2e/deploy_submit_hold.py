"""Hold the deploy create button while its first POST is in flight.

setSubmitting runs before createDeployment. The real server answers after
continue_. A second click during the loading state must not send another POST.
"""
import re
import time
from urllib.parse import urlsplit

from playwright.sync_api import expect
from platform_token_read_revoke import check

DEPLOYMENTS = "/api/v1/deployments"


def hold_deploy_create(page, report):
    held = []
    pattern = re.compile(re.escape(DEPLOYMENTS) + r"(?:\?|$)")
    ok = page.locator(".deploy-submit-bar .ant-btn-primary")

    def hold_post(route):
        if route.request.method != "POST" or urlsplit(route.request.url).path != DEPLOYMENTS:
            route.continue_()
            return
        held.append(route)

    page.route(pattern, hold_post)
    ok.click()
    report["last_step"] = "deploy-create-inflight"
    expect(ok).to_have_class(re.compile(r"\bant-btn-loading\b"))
    ok.click()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline and not held:
        page.wait_for_timeout(100)
    check(len(held) == 1, "duplicate deployment create while the first request is held")
    with page.expect_response(lambda response: urlsplit(response.url).path == DEPLOYMENTS
                              and response.request.method == "POST") as received:
        held[0].continue_()
    page.unroute(pattern, hold_post)
    report["evidence"].append({"layer": "ui", "inflight_posts": 1, "confirm_loading": True})
    return received.value
