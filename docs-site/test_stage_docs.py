import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

import stage_docs
from stage_docs import how_to_groups


class HowToGroupsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.source = Path(self.temp.name)
        (self.source / "how-to").mkdir()

    def page(self, name, title="正文标题"):
        path = self.source / "how-to" / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(f"# {title}\n\n正文保持不变。\n", encoding="utf-8")

    def index(self, body):
        (self.source / "README.md").write_text(
            "# 文档\n\n### 操作指南（how-to）\n\n" + body,
            encoding="utf-8",
        )

    def test_preserves_source_order_and_labels_without_rewriting_pages(self):
        self.page("a.md", "A 页正文标题")
        self.page("z.md", "Z 页正文标题")
        self.page("nested/m.md", "M 页正文标题")
        self.index(
            "**第二组**\n\n"
            "- [Z 导航标签](how-to/z.md)\n"
            "- [A 导航标签](how-to/a.md)\n\n"
            "**第一组**\n\n"
            "- [M 导航标签](how-to/nested/m.md)\n\n"
            "## 查表入口\n\n"
            "**不属于操作指南**\n"
            "- [外部章节的重复入口](how-to/z.md)\n"
        )
        before = {p: p.read_bytes() for p in self.source.rglob("*.md")}

        self.assertEqual(how_to_groups(self.source), [
            {"title": "第二组", "pages": [
                {"title": "Z 导航标签", "path": "how-to/z.md"},
                {"title": "A 导航标签", "path": "how-to/a.md"},
            ]},
            {"title": "第一组", "pages": [
                {"title": "M 导航标签", "path": "how-to/nested/m.md"},
            ]},
        ])
        self.assertEqual(before, {p: p.read_bytes() for p in self.source.rglob("*.md")})

    def test_unindexed_page_fails_instead_of_disappearing_from_navigation(self):
        self.page("a.md")
        self.page("new.md")
        self.index("**组**\n- [A](how-to/a.md)\n")
        with self.assertRaisesRegex(ValueError, "missing.*how-to/new.md"):
            how_to_groups(self.source)

    def test_duplicate_page_across_groups_fails(self):
        self.page("a.md")
        self.index("**组一**\n- [A](how-to/a.md)\n**组二**\n- [别名](how-to/a.md)\n")
        with self.assertRaisesRegex(ValueError, "duplicate how-to page: how-to/a.md"):
            how_to_groups(self.source)

    def test_missing_target_reports_the_broken_link(self):
        self.index("**组**\n- [不存在](how-to/missing.md)\n")
        with self.assertRaisesRegex(ValueError, "does not exist: how-to/missing.md"):
            how_to_groups(self.source)

    def test_invalid_paths_cannot_change_the_section_or_escape_it(self):
        for path in (
            "../outside.md", "how-to/../outside.md", "/how-to/a.md",
            "reference/a.md", "https://example.com/a.md", "how-to/a.md#part",
            "how-to/a.md?download", "how-to/./a.md", "how-to//a.md",
            "how-to/a\\b.md",
        ):
            with self.subTest(path=path):
                self.index(f"**组**\n- [页面]({path})\n")
                with self.assertRaisesRegex(ValueError, "invalid how-to page path"):
                    how_to_groups(self.source)

    def test_link_before_a_group_and_empty_group_fail(self):
        self.page("a.md")
        self.index("- [A](how-to/a.md)\n")
        with self.assertRaisesRegex(ValueError, "before a group"):
            how_to_groups(self.source)
        self.index("**空组**\n**有内容**\n- [A](how-to/a.md)\n")
        with self.assertRaisesRegex(ValueError, "nonempty groups"):
            how_to_groups(self.source)

    def test_section_index_is_not_required_to_be_a_grouped_page(self):
        self.page("README.md")
        self.page("a.md")
        self.index("**组**\n- [A](how-to/a.md)\n")
        self.assertEqual(how_to_groups(self.source)[0]["pages"], [
            {"title": "A", "path": "how-to/a.md"},
        ])

    def test_missing_or_duplicate_how_to_section_fails(self):
        self.index("**组**\n### 操作指南（how-to）\n")
        with self.assertRaisesRegex(ValueError, "one how-to section"):
            how_to_groups(self.source)
        (self.source / "README.md").write_text("# 无操作指南\n", encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "one how-to section"):
            how_to_groups(self.source)

    def test_main_publishes_group_order_without_changing_page_identity(self):
        self.page("a.md", "A 正文标题")
        self.page("z.md", "Z 正文标题")
        self.page("nested/m.md", "M 正文标题")
        self.index(
            "**第二组**\n- [Z 导航标签](how-to/z.md)\n- [A 导航标签](how-to/a.md)\n"
            "**第一组**\n- [M 导航标签](how-to/nested/m.md)\n"
        )
        source_before = {p: p.read_bytes() for p in self.source.rglob("*.md")}
        expected_groups = [
            {"title": "第二组", "pages": [
                {"title": "Z 导航标签", "path": "how-to/z.md"},
                {"title": "A 导航标签", "path": "how-to/a.md"},
            ]},
            {"title": "第一组", "pages": [
                {"title": "M 导航标签", "path": "how-to/nested/m.md"},
            ]},
        ]

        with tempfile.TemporaryDirectory() as output:
            dest = Path(output) / "content"
            with patch.multiple(
                stage_docs, SOURCE=self.source, DEST=dest,
                STATIC_ASSETS=Path(output) / "static" / "assets",
            ), redirect_stdout(io.StringIO()):
                stage_docs.main()

            front, body = (dest / "how-to" / "_index.md").read_text().split("\n---\n", 1)
            fields = dict(line.split(": ", 1) for line in front.splitlines()[1:])
            self.assertEqual(json.loads(fields["guide_groups"]), expected_groups)
            self.assertTrue(json.loads(fields["no_list"]))
            self.assertNotIn("sidebar_root_for", fields)
            self.assertEqual(json.loads(fields["weight"]), 2)
            self.assertIn("weight: 1\n", (dest / "tutorials" / "_index.md").read_text())
            self.assertEqual(body.lstrip("\n"), (
                "## 第二组\n\n- [Z 导航标签](z/)\n- [A 导航标签](a/)\n\n"
                "## 第一组\n\n- [M 导航标签](nested/m/)\n"
            ))
            self.assertEqual(
                {p.relative_to(dest / "how-to").as_posix() for p in (dest / "how-to").rglob("*.md")},
                {"_index.md", "z.md", "a.md", "nested/m.md"},
            )
            for path, title, label, weight in (
                ("z.md", "Z 正文标题", "Z 导航标签", 1),
                ("a.md", "A 正文标题", "A 导航标签", 2),
                ("nested/m.md", "M 正文标题", "M 导航标签", 3),
            ):
                with self.subTest(path=path):
                    front, body = (dest / "how-to" / path).read_text().split("\n---\n", 1)
                    fields = dict(line.split(": ", 1) for line in front.splitlines()[1:])
                    self.assertEqual(json.loads(fields["title"]), title)
                    self.assertEqual(json.loads(fields["linkTitle"]), label)
                    self.assertEqual(json.loads(fields["weight"]), weight)
                    self.assertEqual(body.lstrip("\n"), "正文保持不变。\n")
        self.assertEqual(source_before, {p: p.read_bytes() for p in self.source.rglob("*.md")})

    def test_main_keeps_previous_output_when_a_page_has_no_group(self):
        self.page("a.md")
        self.page("unindexed.md")
        self.index("**组**\n- [A](how-to/a.md)\n")
        with tempfile.TemporaryDirectory() as output:
            output = Path(output)
            dest = output / "content"
            assets = output / "static" / "assets"
            dest.mkdir()
            assets.mkdir(parents=True)
            (dest / "_index.md").write_text("上次成功生成的入口\n", encoding="utf-8")
            (assets / "existing.svg").write_text("<svg/>\n", encoding="utf-8")
            before = {p: p.read_bytes() for p in output.rglob("*") if p.is_file()}
            with patch.multiple(stage_docs, SOURCE=self.source, DEST=dest, STATIC_ASSETS=assets):
                with self.assertRaisesRegex(ValueError, "missing.*how-to/unindexed.md"):
                    stage_docs.main()
            self.assertEqual(before, {p: p.read_bytes() for p in output.rglob("*") if p.is_file()})


if __name__ == "__main__":
    unittest.main()
