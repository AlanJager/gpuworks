"""Offline package-fixture contract, not browser or deployment evidence.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import io
import tarfile
import unittest
from template_catalog_fixture import catalog_package


class CatalogFixtureTests(unittest.TestCase):
    def test_owned_package_is_deterministic_and_self_contained(self):
        args = ("e2e-run/template", "e2e-run-spec", "e2e-run")
        raw = catalog_package(*args)
        self.assertEqual(raw, catalog_package(*args))
        with tarfile.open(fileobj=io.BytesIO(raw), mode="r:gz") as archive:
            self.assertEqual(archive.getnames(), ["manifest.yaml", "models/e2e-run/template/README.en.md", "models/e2e-run/template/model.yaml"])
            model = archive.extractfile("models/e2e-run/template/model.yaml").read().decode()
            self.assertIn("model_id: e2e-run/template\n", model)
            self.assertIn("weight_bytes: 1048576\n", model)
            self.assertIn('args: ["--max-model-len", "4096"]', model)
            self.assertIn("readme_en: README.en.md", model)
            self.assertTrue(all(item.isfile() and item.mtime == 0 for item in archive))

    def test_rejects_unowned_or_escaping_identity(self):
        for identity in ["upstream/model", "e2e-x/../../victim", "e2e-UPPER/template"]:
            with self.subTest(identity=identity), self.assertRaises(ValueError):
                catalog_package(identity, "e2e-spec", "e2e-title")


if __name__ == "__main__":
    unittest.main()
