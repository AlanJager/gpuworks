import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
VERSION = re.compile(r"v(\d+)\.(\d+)\.(\d+)")


def listed_versions(index: Path) -> list[str]:
    """Versions in the order the index links them, each once."""
    seen = []
    for name in re.findall(r"\]\((v\d+\.\d+\.\d+)\.md\)", index.read_text(encoding="utf-8")):
        if name not in seen:
            seen.append(name)
    return seen


def key(version: str) -> tuple[int, ...]:
    return tuple(int(part) for part in VERSION.fullmatch(version).groups())


class ReleaseNotesTest(unittest.TestCase):
    """The published release notes stay one consistent record of every version."""

    def setUp(self):
        self.source = ROOT / "docs" / "releases"
        self.versions = listed_versions(self.source / "README.md")

    def test_every_page_is_listed_and_every_listed_version_has_both_pages(self):
        pages = sorted(path.stem for path in self.source.glob("v*.md"))
        self.assertEqual(sorted(self.versions), pages)
        for version in self.versions:
            self.assertTrue((ROOT / "docs-en" / "releases" / f"{version}.md").is_file(), version)
        self.assertEqual(listed_versions(ROOT / "docs-en" / "releases" / "README.md"), self.versions)

    def test_versions_are_listed_newest_first(self):
        self.assertTrue(self.versions)
        self.assertEqual(self.versions, sorted(self.versions, key=key, reverse=True))

    def test_each_page_is_titled_by_its_version(self):
        for language in ("docs", "docs-en"):
            for version in self.versions:
                first = (ROOT / language / "releases" / f"{version}.md").read_text(encoding="utf-8").splitlines()[0]
                self.assertEqual(first, f"# {version}", f"{language}/releases/{version}.md")

    def test_full_changelog_compares_with_the_previous_version(self):
        for language in ("docs", "docs-en"):
            for newer, older in zip(self.versions, self.versions[1:]):
                body = (ROOT / language / "releases" / f"{newer}.md").read_text(encoding="utf-8")
                self.assertIn(f"/compare/{older}...{newer})", body, f"{language}/releases/{newer}.md")


if __name__ == "__main__":
    unittest.main()
