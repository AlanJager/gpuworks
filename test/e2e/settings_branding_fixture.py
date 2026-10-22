"""Error evidence for the branding case's anonymous sign-in context.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
from urllib.parse import urlsplit


def watch_public_page(page):
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))

    def console_seen(message):
        if message.type != "error":
            return
        path = urlsplit(message.location.get("url", "")).path
        if (path == "/api/v1/auth/me" and "401" in message.text
                and ("load resource" in message.text or "加载资源" in message.text)):
            return
        errors.append(message.text)

    def response_seen(response):
        path = urlsplit(response.url).path
        if response.status >= 400:
            if path == "/api/v1/auth/me" and response.status == 401:
                return
            errors.append(f"{response.status} {path}")

    page.on("console", console_seen)
    page.on("response", response_seen)
    return errors
