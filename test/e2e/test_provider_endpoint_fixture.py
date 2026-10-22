"""RunPod v2 external boundary self-tests (not browser coverage).

Run: PYTHONPATH=test/e2e python3 -m unittest test_provider_endpoint_fixture -v
E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import json
import unittest
import urllib.error
import urllib.request

from provider_endpoint_fixture import RunPodFixture


KEY_A = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILXGDN/SclOozk1xsDztpmhGiKkkrfQB9SKoO8dSIQQZ existing@example.com"
KEY_B = KEY_A.replace("existing@example.com", "replacement@example.com")


class RunPodFixtureTest(unittest.TestCase):
    def setUp(self):
        self.fixture = RunPodFixture("fixture-api-key")
        self.fixture.__enter__()
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def tearDown(self):
        self.fixture.__exit__(None, None, None)
        self.assertFalse(self.fixture.thread.is_alive())

    def request(self, path="/proxy-a/v2/account/ssh-keys", method="GET", body=None,
                authorization="Bearer fixture-api-key", raw=None):
        headers = {"Content-Type": "application/json"}
        if authorization is not None:
            headers["Authorization"] = authorization
        data = raw if raw is not None else json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.fixture.base + path, method=method, data=data, headers=headers)
        try:
            response = self.client.open(request, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers.get_content_type(), json.load(response)

    # Mainline: missing, malformed and invalid bearer credentials cannot read or replace keys.
    def test_authentication_errors_preserve_keys(self):
        for authorization in (None, "fixture-api-key", "Bearer wrong-key"):
            for method in ("GET", "PUT"):
                with self.subTest(authorization=authorization, method=method):
                    status, content_type, body = self.request(method=method, body={"keys": [KEY_A]} if method == "PUT" else None,
                                                              authorization=authorization)
                    self.assertEqual(status, 401)
                    self.assertEqual(content_type, "application/problem+json")
                    self.assertEqual(set(body), {"title", "status", "detail"})
                    self.assertEqual((body["title"], body["status"]), ("Unauthorized", 401))
        self.assertEqual(self.request()[2], {"keys": []})

    # Mainline: invalid JSON, missing required keys, wrong JSON types and unknown fields fail closed.
    def test_invalid_replacements_do_not_change_the_account(self):
        self.assertEqual(self.request(method="PUT", body={"keys": [KEY_A]})[0], 200)
        for body in ({}, [], {"keys": None}, {"keys": "key"}, {"keys": [False]}, {"keys": [42]},
                     {"keys": ["bad-key"]}, {"keys": ["ssh-ed25519 abc\nmalformed"]},
                     {"keys": [], "extra": True}):
            with self.subTest(body=body):
                status, content_type, response = self.request(method="PUT", body=body)
                self.assertEqual(status, 422)
                self.assertEqual(content_type, "application/problem+json")
                self.assertEqual((response["title"], response["status"]), ("Unprocessable Entity", 422))
                self.assertEqual(self.request()[2], {"keys": [KEY_A]})
        status, content_type, response = self.request(method="PUT", raw=b"{")
        self.assertEqual((status, content_type, response["title"]), (400, "application/problem+json", "Bad Request"))
        self.assertEqual(self.request()[2], {"keys": [KEY_A]})

    # Mainline: a scoped key can be forbidden independently of an invalid/expired key.
    def test_forbidden_and_forced_auth_rejection_recover(self):
        self.fixture.forbidden = True
        self.assertEqual(self.request()[0:2], (403, "application/problem+json"))
        self.fixture.forbidden = False
        self.fixture.reject = True
        self.assertEqual(self.request()[0], 401)
        self.fixture.reject = False
        self.assertEqual(self.request()[2], {"keys": []})

    # Mainline: initial empty keys/pods have the v2 envelopes; PUT replaces the full account list.
    def test_empty_defaults_and_complete_replacement(self):
        self.assertEqual(self.request()[2], {"keys": []})
        self.assertEqual(self.request("/proxy-a/v2/pods")[2],
                         {"pods": [], "pagination": {"nextCursor": None, "hasNextPage": False}})
        for keys in ([KEY_A, KEY_B], [KEY_B], []):
            self.assertEqual(self.request(method="PUT", body={"keys": keys}), (200, "application/json", {"keys": keys}))
            self.assertEqual(self.request("/proxy-b/v2/account/ssh-keys")[2], {"keys": keys})

    # Mainline: the fixture's selected route/method subset never accepts an unknown call.
    def test_unknown_routes_and_methods_fail_closed(self):
        for path in ("/api/v1/providers", "/proxy-a/v2/account/unknown", "/proxy-c/v2/pods"):
            self.assertEqual(self.request(path)[0], 404)
        for method in ("POST", "DELETE", "PATCH"):
            self.assertEqual(self.request(method=method)[0], 405)
        self.assertEqual([r["status"] for r in self.fixture.requests], [404, 404, 404, 405, 405, 405])


if __name__ == "__main__":
    unittest.main()
