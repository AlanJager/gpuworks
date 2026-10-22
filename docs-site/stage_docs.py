#!/usr/bin/env python3
"""Stage the repository's Chinese docs into the OINK content tree.

The Markdown under ../docs remains the only source. Hugo gets a generated
copy so the existing files do not need front matter or Hugo-specific syntax.
"""

from __future__ import annotations

import json
import re
import shutil
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "docs"
DEST = Path(__file__).resolve().parent / "content"
STATIC_ASSETS = Path(__file__).resolve().parent / "static" / "assets"

SECTION_META = {
    "tutorials": ("用户教程", "从零启动 GPUWorks，并完成第一个模型部署。"),
    "how-to": ("操作指南", "针对安装、部署、同步、监控和生产环境的任务型文档。"),
    "reference": ("参考手册", "API、CLI、配置、支持矩阵和安全边界的精确说明。"),
    "explanation": ("架构说明", "解释 GPUWorks 的连接、调度、显存估算和 dGPU 设计。"),
    "dev": ("开发者文档", "本地搭建、测试、代码地图、发布和贡献约定。"),
    "adr": ("架构决策记录", "已接受的长期架构取舍和协议边界。"),
}
SECTION_WEIGHTS = {slug: weight for weight, slug in enumerate(SECTION_META, start=1)}


def how_to_groups(source_dir: Path) -> list[dict]:
    """Read navigation labels and order from the documentation index."""
    lines = (source_dir / "README.md").read_text(encoding="utf-8").splitlines()
    starts = [
        index for index, line in enumerate(lines)
        if re.fullmatch(r"###\s+.+[（(]how-to[）)]\s*", line)
    ]
    if len(starts) != 1:
        raise ValueError("docs/README.md must contain one how-to section")

    groups = []
    seen_paths = set()
    seen_titles = set()
    for line in lines[starts[0] + 1:]:
        if re.match(r"#{1,3}\s", line):
            break
        group = re.fullmatch(r"\s*\*\*(.+?)\*\*\s*", line)
        if group:
            title = group.group(1).strip()
            if not title or title in seen_titles:
                raise ValueError(f"empty or duplicate how-to group: {title!r}")
            groups.append({"title": title, "pages": []})
            seen_titles.add(title)
            continue

        entry = re.match(r"\s*[-*+]\s+\[([^\]]+)\]\(([^)]+)\)", line)
        if not entry:
            continue
        title, path = entry.group(1), entry.group(2).strip()
        if not groups:
            raise ValueError(f"how-to page appears before a group: {path}")
        if (
            not path.startswith("how-to/")
            or not path.endswith(".md")
            or any(part in {"", ".", ".."} for part in path.split("/"))
            or any(char in path for char in "\\?#")
            or Path(path).name in {"README.md", "_index.md"}
        ):
            raise ValueError(f"invalid how-to page path: {path}")
        target = source_dir / path
        if not target.resolve().is_relative_to((source_dir / "how-to").resolve()):
            raise ValueError(f"how-to page escapes its source directory: {path}")
        if not target.is_file():
            raise ValueError(f"how-to page does not exist: {path}")
        if path in seen_paths:
            raise ValueError(f"duplicate how-to page: {path}")
        seen_paths.add(path)
        groups[-1]["pages"].append({"title": title, "path": path})

    if not groups or any(not group["pages"] for group in groups):
        raise ValueError("how-to navigation must contain nonempty groups")
    pages = {
        path.relative_to(source_dir).as_posix()
        for path in (source_dir / "how-to").rglob("*.md")
        if path.is_file() and path.name not in {"README.md", "_index.md"}
    }
    missing = pages - seen_paths
    if missing:
        raise ValueError(f"how-to pages missing from docs/README.md: {', '.join(sorted(missing))}")
    return groups


def quote(value: str) -> str:
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def first_heading(text: str, fallback: str) -> str:
    match = re.search(r"(?m)^#\s+(.+?)\s*$", text)
    if not match:
        return fallback.replace("-", " ").replace("_", " ").title()
    return re.sub(r"[`*_]", "", match.group(1)).strip()


def add_front_matter(text: str, title: str, **metadata) -> str:
    fields = "".join(f"{key}: {json.dumps(value, ensure_ascii=False)}\n" for key, value in metadata.items())
    if text.startswith("---\n"):
        return "---\n" + fields + text[4:]
    return "---\n" + f"title: {quote(title)}\n" + "type: docs\n" + fields + "---\n\n" + text


def how_to_index(groups: list[dict]) -> str:
    sections = []
    for group in groups:
        links = [
            f'- [{page["title"]}]({page["path"].removeprefix("how-to/").removesuffix(".md")}/)'
            for page in group["pages"]
        ]
        sections.append(f'## {group["title"]}\n\n' + "\n".join(links))
    return add_front_matter(
        "\n\n".join(sections) + "\n", "操作指南",
        description=SECTION_META["how-to"][1], no_list=True,
        weight=SECTION_WEIGHTS["how-to"], guide_groups=groups,
    )


def remove_leading_heading(text: str) -> str:
    """Let OINK render the front matter title once instead of twice."""
    return re.sub(r"\A\s*#\s+[^\n]+\n+", "", text, count=1)


IMAGE_RE = re.compile(r"(!\[[^\]]*\]\()((?:\.\./)*(?:\./)?assets/[^)\s]+)(\))")
ASSET_LINK_RE = re.compile(r"(?<!!)(\[[^\]]*\]\()((?:\.\./)*(?:\./)?assets/[^)\s]+)(\))")


def rewrite_images(text: str) -> str:
    """Point ![](../assets/x.png) at the published static copy.

    Hugo serves pages from directory-style URLs, so a relative image path that
    works on GitHub would resolve one level too deep on the site. The OINK
    image render hook prepends the site's base path to root-relative paths, so
    the rewrite must not include it.
    """
    return IMAGE_RE.sub(
        lambda m: m.group(1) + "/assets/" + m.group(2).split("assets/", 1)[1] + m.group(3),
        text,
    )


def rewrite_asset_links(text: str, source: Path) -> str:
    """Resolve downloads from directory-style page URLs under any base path."""
    levels = len(source.parts) - (source.name in {"README.md", "_index.md"})
    prefix = "../" * levels
    return ASSET_LINK_RE.sub(
        lambda m: m.group(1) + prefix + "assets/" + m.group(2).split("assets/", 1)[1] + m.group(3),
        text,
    )


def home_page() -> str:
    return (
        "---\n"
        'title: "GPUWorks 文档"\n'
        'description: "从第一台 GPU，到可用的推理服务。"\n'
        "type: docs\n"
        "---\n\n"
        "GPUWorks 是一个面向 GPU 集群的轻量管理平台。这里汇总当前版本的教程、操作指南、"
        "参考手册、架构说明和开发者文档。\n\n"
        "## 推荐入口\n\n"
        "- [从零启动并部署第一个模型](tutorials/getting-started/)\n"
        "- [接入节点并检查 GPU](how-to/connect-nodes/)\n"
        "- [部署模型并调用 OpenAI 兼容 API](how-to/deploy-models/)\n"
        "- [遇到问题：排障指南](how-to/troubleshooting/)\n"
        "- [按分类浏览操作指南](how-to/)\n"
        "- [查看 API、CLI 和配置参考](reference/)\n"
    )


def main() -> None:
    if not SOURCE.is_dir():
        raise SystemExit(f"missing documentation source: {SOURCE}")
    groups = how_to_groups(SOURCE)
    navigation = {
        page["path"]: {"linkTitle": page["title"], "weight": weight}
        for weight, page in enumerate(
            (page for group in groups for page in group["pages"]), start=1
        )
    }
    shutil.rmtree(DEST, ignore_errors=True)
    DEST.mkdir(parents=True)

    shutil.rmtree(STATIC_ASSETS, ignore_errors=True)
    if (SOURCE / "assets").is_dir():
        shutil.copytree(SOURCE / "assets", STATIC_ASSETS)

    for source in sorted(SOURCE.rglob("*.md")):
        relative = source.relative_to(SOURCE)
        target_relative = relative
        if relative.name == "README.md":
            target_relative = relative.with_name("_index.md")
        target = DEST / target_relative
        target.parent.mkdir(parents=True, exist_ok=True)
        body = source.read_text(encoding="utf-8")
        if relative == Path("README.md"):
            target.write_text(home_page(), encoding="utf-8")
            continue
        body = rewrite_images(body)
        body = rewrite_asset_links(body, relative)
        title = first_heading(body, target.stem)
        metadata = navigation.get(relative.as_posix(), {})
        if relative.name == "README.md" and relative.parent.as_posix() in SECTION_WEIGHTS:
            metadata = {"weight": SECTION_WEIGHTS[relative.parent.as_posix()]}
        target.write_text(
            add_front_matter(remove_leading_heading(body), title, **metadata),
            encoding="utf-8",
        )

    (DEST / "how-to" / "_index.md").write_text(how_to_index(groups), encoding="utf-8")

    for slug, (title, description) in SECTION_META.items():
        section = DEST / slug / "_index.md"
        if section.exists():
            continue
        section.parent.mkdir(parents=True, exist_ok=True)
        section.write_text(
            "---\n"
            f"title: {quote(title)}\n"
            f"description: {quote(description)}\n"
            f"weight: {SECTION_WEIGHTS[slug]}\n"
            "type: docs\n---\n",
            encoding="utf-8",
        )

    count = sum(1 for _ in DEST.rglob("*.md"))
    print(f"staged {count} Markdown pages from {SOURCE}")


if __name__ == "__main__":
    main()
