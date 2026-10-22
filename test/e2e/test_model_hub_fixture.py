"""Strict external protocol/observation checks; not browser business evidence."""
import hashlib
import json
import unittest
from urllib.error import HTTPError
from urllib.request import Request, build_opener, ProxyHandler

from model_hub_fixture import CONFIG, INDEX, README, SHA, WEIGHT_BYTES, ModelHub


class ModelHubContract(unittest.TestCase):
    def setUp(self):
        self.hub = ModelHub("e2e-fixture/model")

    def test_refuses_non_owned_identity_and_untyped_availability(self):
        for value in ("org/model", "e2e-foo/../model", "e2e-f/model/extra"):
            with self.assertRaises(ValueError):
                ModelHub(value)
        for value in (1, "false", None):
            with self.assertRaises(ValueError):
                self.hub.available(value)

    def test_negative_method_body_credentials_query_and_revision(self):
        root = "/hf/api/models/e2e-fixture/model"
        for method in ("POST", "PUT", "DELETE", "HEAD"):
            self.assertEqual(self.hub.answer(method, root)[0], 405)
        for headers in ({"Authorization": "Bearer synthetic"}, {"Cookie": "test=yes"}):
            self.assertEqual(self.hub.answer("GET", root, headers)[0], 400)
        self.assertEqual(self.hub.answer("GET", root, body=b"{}")[0], 400)
        for target in (root + "?blobs=1", root + "?blobs=true&blobs=true",
            "/ms/api/v1/models/e2e-fixture/model/repo/files?Recursive=false",
            "/ms/api/v1/models/e2e-fixture/model/repo/files?Recursive=true&Revision=main",
            "/ms/api/v1/models/e2e-fixture/model/repo?Revision=master&FilePath=config.json"):
            with self.subTest(target=target):
                self.assertEqual(self.hub.answer("GET", target)[0], 400)
        for target in ("/hf/e2e-fixture/model/resolve/other/config.json", "/api/v1/models",
                       "/hf/other/model/raw/main/config.json"):
            self.assertEqual(self.hub.answer("GET", target)[0], 404)

    def test_exact_hf_ms_files_and_revision_records(self):
        for base in ("/hf/e2e-fixture/model/raw/main/", "/hf/e2e-fixture/model/resolve/" + SHA + "/",
                     "/ms/models/e2e-fixture/model/resolve/master/"):
            for file, value in (("config.json", CONFIG), ("README.md", README), ("model.safetensors.index.json", INDEX)):
                self.assertEqual(self.hub.answer("GET", base + file), (200, value.encode()))
        hf = json.loads(self.hub.answer("GET", "/hf/api/models/e2e-fixture/model?blobs=true")[1])
        self.assertEqual(hf["sha"], SHA)
        self.assertEqual(hf["siblings"], [{"rfilename": "model.safetensors", "size": WEIGHT_BYTES}])
        ms = json.loads(self.hub.answer("GET", "/ms/api/v1/models/e2e-fixture/model/repo/files?Recursive=true")[1])
        self.assertEqual(ms["Code"], 200)
        self.assertEqual(ms["Data"]["Files"][0]["Revision"], SHA)
        self.assertEqual(self.hub.answer("GET", "/ms/api/v1/models/e2e-fixture/model/repo?FilePath=config.json"), (200, CONFIG.encode()))

    def test_real_http_cors_audit_failure_recovery_and_observed_stop(self):
        base = self.hub.start()
        try:
            opener = build_opener(ProxyHandler({}))
            target = "/hf/e2e-fixture/model/raw/main/config.json"
            request = Request(base + target, headers={"Origin": "http://127.0.0.1:8800"})
            with opener.open(request, timeout=3) as response:
                self.assertEqual(response.read(), CONFIG.encode())
                self.assertEqual(response.headers["Access-Control-Allow-Origin"], "http://127.0.0.1:8800")
            self.assertEqual(self.hub.audit()[0]["sha256"], hashlib.sha256(CONFIG.encode()).hexdigest())
            self.hub.available(False)
            with self.assertRaises(HTTPError) as failure:
                opener.open(request, timeout=3)
            self.assertEqual(failure.exception.code, 503)
            failure.exception.close()
            self.hub.available(True)
            with opener.open(request, timeout=3) as response:
                self.assertEqual(response.status, 200)
            bad = Request(base + target, headers={"Origin": "https://foreign.example"})
            with self.assertRaises(HTTPError) as failure:
                opener.open(bad, timeout=3)
            self.assertEqual(failure.exception.code, 403)
            failure.exception.close()
            self.assertEqual([r["status"] for r in self.hub.audit()], [200, 503, 200, 403])
        finally:
            self.assertEqual(self.hub.stop(), {"listener_stopped": True, "thread_stopped": True,
                                             "handlers": 0, "stopped": True})
        self.assertTrue(self.hub.stop()["stopped"])


if __name__ == "__main__":
    unittest.main()
