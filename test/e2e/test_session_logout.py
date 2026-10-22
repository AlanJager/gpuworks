"""Security boundaries of the real-client session E2E fixture.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import json
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import tempfile
from threading import Thread
import unittest

from playwright.sync_api import sync_playwright
from local_browser_fixture import Artifacts, http
from session_logout import COOKIE, LOGIN, capture_auth_response, capture_credentials, cookie_identity


class CredentialIsolationTests(unittest.TestCase):
    def test_cookie_and_bearer_clients_never_inherit_each_others_credentials(self):
        received = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                received.append({"cookie": self.headers.get("Cookie"), "bearer": self.headers.get("Authorization")})
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                # If clients reused a cookie jar this poison would contaminate
                # the next probe and could make revoked credentials seem valid.
                self.send_header("Set-Cookie", COOKIE + "=replacement; Path=/")
                self.end_headers()
                self.wfile.write(json.dumps({"username": "fixture"}).encode())

            def log_message(self, *_args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = Thread(target=server.serve_forever, daemon=True)
        thread.start()
        base = f"http://127.0.0.1:{server.server_port}"
        try:
            with sync_playwright() as play:
                self.assertEqual(cookie_identity(play, base, "original")[0], 200)
                self.assertEqual(cookie_identity(play, base, "original")[0], 200)
            self.assertEqual(http(base, "GET", "/api/v1/auth/me", "login-bearer")[0], 200)
            self.assertEqual(http(base, "GET", "/api/v1/auth/me")[0], 200)
            self.assertEqual(received, [
                {"cookie": COOKIE + "=original", "bearer": None},
                {"cookie": COOKIE + "=original", "bearer": None},
                {"cookie": None, "bearer": "Bearer login-bearer"},
                {"cookie": None, "bearer": None},
            ])
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_response_barrier_preserves_original_response_and_releases_on_failure(self):
        calls = []
        bearer = "captured.real.login-bearer"

        class CDP:
            def send(self, method, params):
                calls.append((method, params))
                if method == "Fetch.getResponseBody":
                    return {"base64Encoded": True, "body": base64.b64encode(json.dumps({"token": bearer}).encode()).decode()}

        event = {"requestId": "request-1", "request": {"url": "http://127.0.0.1" + LOGIN, "method": "POST"}, "responseStatusCode": 200}
        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / "case")
            self.assertEqual(capture_auth_response(CDP(), event, artifacts), (LOGIN, {"status": 200, "body": {"token": bearer}}))
            self.assertEqual(calls, [("Fetch.getResponseBody", {"requestId": "request-1"}),
                                    ("Fetch.continueResponse", {"requestId": "request-1"})])
            self.assertNotIn(bearer, artifacts.redact(bearer))
            calls.clear()
            event["request"]["url"] = "http://127.0.0.1/api/v1/settings"
            with self.assertRaisesRegex(AssertionError, "unexpected auth capture target"):
                capture_auth_response(CDP(), event, artifacts)
            self.assertEqual(calls, [("Fetch.continueResponse", {"requestId": "request-1"})])

    def test_malformed_login_registers_secrets_before_failing(self):
        cookie, bearer = "c" * 64, "sensitive.login.bearer"

        class Response:
            status = 200
            def json(self):
                return {"token": bearer, "unexpected": True}

        class Context:
            def cookies(self):
                return [{"name": COOKIE, "value": cookie}]

        with tempfile.TemporaryDirectory() as directory:
            artifacts = Artifacts(Path(directory) / "case")
            with self.assertRaisesRegex(AssertionError, "response contract"):
                capture_credentials(Response(), Context(), artifacts, Response().json())
            self.assertNotIn(cookie, artifacts.redact(cookie))
            self.assertNotIn(bearer, artifacts.redact(bearer))


if __name__ == "__main__":
    unittest.main()
