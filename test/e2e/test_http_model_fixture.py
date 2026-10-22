"""Contract checks for the external directory used by browser registration cases."""
import json
import unittest
import urllib.error
import urllib.request

from http_model_fixture import ModelDirectory


class ModelDirectoryTest(unittest.TestCase):
    def setUp(self):
        self.fixture = ModelDirectory("unit").start()
        self.addCleanup(self.fixture.stop)
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(self, path, method="GET", headers=None):
        request = urllib.request.Request(self.fixture.url + path, method=method, headers=headers or {})
        try:
            response = self.client.open(request, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers, response.read()

    def test_index_config_exact_head_and_range(self):
        status, headers, body = self.request("/")
        self.assertEqual(status, 200)
        self.assertIn("text/html", headers["Content-Type"])
        self.assertIn(b'href="config.json"', body)
        self.assertEqual(json.loads(self.request("/config.json")[2]), self.fixture.config)
        status, headers, body = self.request("/model.safetensors", "HEAD")
        self.assertEqual((status, body), (200, b""))
        self.assertEqual(int(headers["Content-Length"]), len(self.fixture.files["model.safetensors"]))
        status, headers, body = self.request("/model.safetensors", headers={"Range": "bytes=0-4"})
        self.assertEqual((status, body), (206, b"fixtu"))
        self.assertTrue(headers["Content-Range"].startswith("bytes 0-4/"))
        self.assertEqual(len(self.fixture.audit()), 4)

    def test_errors_outage_and_restore_are_real_http(self):
        self.assertEqual(self.request("/absent")[0], 404)
        self.assertEqual(self.request("/", "POST")[0], 405)
        self.assertEqual(self.request("/config.json", headers={"Range": "bytes=99999-"})[0], 416)
        self.fixture.set_available(False)
        self.assertEqual(self.request("/")[0], 503)
        self.fixture.set_available(True)
        self.assertEqual(self.request("/")[0], 200)
        self.assertEqual([item["status"] for item in self.fixture.audit()], [404, 405, 416, 503, 200])


if __name__ == "__main__":
    unittest.main()
