"""Re-entered auth pages must match an independent session query.

The saved login is the identity returned by POST /api/v1/auth/login. After that
save, reloading the networks page and opening the settings deep link must show
those pages, and a cookie-only client plus a bearer-only client must read the
same identity from GET /api/v1/auth/me. A management session has no agent.
"""
from __future__ import annotations

from playwright.sync_api import expect

from local_browser_fixture import http
from platform_token_read_revoke import check

ME = "/api/v1/auth/me"
COOKIE = "gpuworks_session"


def _cookie_identity(play, base, value):
    client = play.request.new_context(extra_http_headers={"Cookie": f"{COOKIE}={value}"})
    try:
        response = client.get(base + ME)
        return response.status, response.json()
    finally:
        client.dispose()


def exercise_session_persistence(page, context, play, server, identity, networks, settings, auth, observe, cookie, bearer):
    page.reload()
    refreshed = page.request.get(server.base + ME)
    check(refreshed.status == 200 and refreshed.json() == identity,
          "independent session query after re-entering networks differs from the saved login")
    check(_cookie_identity(play, server.base, cookie) == (200, identity),
          "cookie-only session query after re-entry differs from the saved login")
    check(http(server.base, "GET", ME, bearer) == (200, identity),
          "bearer session query after re-entry differs from the saved login")
    expect(page).to_have_url(server.base + "/networks")
    expect(page.get_by_role("heading", name=networks["title"], exact=True)).to_be_visible()
    expect(page.get_by_label(auth["password"], exact=True)).to_have_count(0)

    peer = context.new_page()
    observe(peer)
    peer.goto(server.base + "/settings")
    expect(peer).to_have_url(server.base + "/settings")
    expect(peer.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    peer.reload()
    deep = peer.request.get(server.base + ME)
    check(deep.status == 200 and deep.json() == identity,
          "independent session query after the settings deep link differs from the saved login")
    expect(peer).to_have_url(server.base + "/settings")
    expect(peer.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    return peer, {
        "reentered_networks": True,
        "deep_link_settings": True,
        "independent_get_matches": True,
        "cookie_client": 200,
        "bearer_client": 200,
        "auth_call": "GET /api/v1/auth/me",
        "agent_or_workload": False,
    }
