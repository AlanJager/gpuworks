"""Public sign-in error monitoring must reject unexpected failures."""
import unittest
from types import SimpleNamespace

from settings_branding_fixture import watch_public_page


class PublicPageErrorsTest(unittest.TestCase):
    def test_only_the_anonymous_auth_probe_is_expected(self):
        callbacks = {}
        page = SimpleNamespace(on=lambda name, handler: callbacks.setdefault(name, handler))
        errors = watch_public_page(page)
        callbacks["console"](SimpleNamespace(type="error", text="Failed to load resource: 401", location={"url": "http://local/api/v1/auth/me"}))
        callbacks["response"](SimpleNamespace(status=401, url="http://local/api/v1/auth/me"))
        self.assertEqual(errors, [])
        callbacks["console"](SimpleNamespace(type="error", text="Failed to load resource: 401", location={"url": "http://local/api/v1/branding"}))
        callbacks["response"](SimpleNamespace(status=500, url="http://local/api/v1/branding"))
        callbacks["pageerror"]("public rendering failed")
        self.assertEqual(errors, ["Failed to load resource: 401", "500 /api/v1/branding", "public rendering failed"])


if __name__ == "__main__":
    unittest.main()
