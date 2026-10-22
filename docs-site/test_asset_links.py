import io
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch
from urllib.parse import urljoin, urlparse

import stage_docs


class AssetLinksTest(unittest.TestCase):
    def test_download_links_resolve_to_copied_assets_at_each_page_depth(self):
        pages = (
            ("how-to/downloads.md", "../", "how-to/downloads/"),
            ("tutorials/basic.md", "../", "tutorials/basic/"),
            ("tutorials/nested/deep.md", "../../", "tutorials/nested/deep/"),
            ("tutorials/README.md", "../", "tutorials/"),
            ("tutorials/nested/README.md", "../../", "tutorials/nested/"),
            ("tutorials/indexed/_index.md", "../../", "tutorials/indexed/"),
        )
        payload = b'{"messages":[{"role":"user","content":"hello"}]}\n'
        external = "[external](https://example.com/assets/prompts.jsonl)"
        with tempfile.TemporaryDirectory() as workspace:
            workspace = Path(workspace)
            source = workspace / "docs"
            asset = source / "assets" / "examples" / "prompts.jsonl"
            asset.parent.mkdir(parents=True)
            asset.write_bytes(payload)
            (source / "README.md").write_text(
                "# 文档\n\n### 操作指南（how-to）\n\n"
                "**附件下载**\n\n- [下载示例](how-to/downloads.md)\n",
                encoding="utf-8",
            )
            for name, prefix, _ in pages:
                path = source / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(
                    f"# 教程\n\n[下载]({prefix}assets/examples/prompts.jsonl)\n\n{external}\n",
                    encoding="utf-8",
                )
            source_before = {
                p: p.read_bytes() for p in source.rglob("*") if p.is_file()
            }
            dest = workspace / "content"
            static = workspace / "static" / "assets"
            with patch.multiple(
                stage_docs, SOURCE=source, DEST=dest, STATIC_ASSETS=static
            ), redirect_stdout(io.StringIO()):
                stage_docs.main()

            for name, _, page_path in pages:
                staged_path = Path(name)
                if staged_path.name == "README.md":
                    staged_path = staged_path.with_name("_index.md")
                staged = (dest / staged_path).read_text(encoding="utf-8")
                href = staged.split("[下载](", 1)[1].split(")", 1)[0]
                for base in ("/", "/docs/"):
                    with self.subTest(page=name, base=base):
                        page_url = f"https://example.com{base}{page_path}"
                        resolved = urlparse(urljoin(page_url, href)).path
                        self.assertEqual(
                            resolved, f"{base}assets/examples/prompts.jsonl"
                        )
                        published = workspace / "static" / resolved.removeprefix(base)
                        self.assertEqual(published.read_bytes(), payload)
                self.assertIn(external, staged)
            self.assertEqual(
                source_before,
                {p: p.read_bytes() for p in source.rglob("*") if p.is_file()},
            )

    def test_download_rewrite_preserves_images_and_other_links(self):
        text = (
            "![本地图](../assets/screenshots/example.png)\n"
            "![外部图](https://example.com/assets/example.png)\n"
            "[外部附件](https://example.com/assets/prompts.jsonl)\n"
            "[站外附件](//example.com/assets/prompts.jsonl)\n"
            "[其他文档](../reference/cli.md)\n"
        )
        self.assertEqual(
            stage_docs.rewrite_asset_links(text, Path("tutorials/basic.md")), text
        )


if __name__ == "__main__":
    unittest.main()
