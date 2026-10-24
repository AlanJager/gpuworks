#!/usr/bin/env python3
"""Check translation coverage and the source revision actually reviewed."""

import argparse
import hashlib
import json
import re
from pathlib import Path

from stage_docs import headings


ROOT = Path(__file__).resolve().parents[1]


def code_blocks(text):
    blocks = []
    current = []
    fence = ""
    for line in text.splitlines():
        marker = re.match(r"^\s*(`{3,}|~{3,})(.*)$", line)
        if not fence and marker:
            fence = marker[1]
            current = [line]
        elif fence:
            current.append(line)
            if marker and marker[1][0] == fence[0] and len(marker[1]) >= len(fence) and not marker[2].strip():
                blocks.append("\n".join(current))
                fence = ""
    if fence:
        raise ValueError("unclosed code fence")
    return blocks


def code_blocks_match(original, translated, root=ROOT):
    source_blocks, translated_blocks = code_blocks(original), code_blocks(translated)
    if len(source_blocks) != len(translated_blocks):
        return False
    review_file = root / "docs-site/localized-code-blocks.json"
    reviews = json.loads(review_file.read_text()) if review_file.exists() else []
    approved = {(item["source_sha256"], item["translation_sha256"]) for item in reviews}
    for source, target in zip(source_blocks, translated_blocks):
        if source == target:
            continue
        pair = tuple(hashlib.sha256(block.encode()).hexdigest() for block in (source, target))
        if pair not in approved:
            return False
    return True


def inspect(root):
    source = root / "docs"
    english = root / "docs-en"
    reviewed = json.loads((root / "docs-site/translations.json").read_text())
    originals = {p.relative_to(source).as_posix(): p for p in source.rglob("*.md")}
    translations = {p.relative_to(english).as_posix(): p for p in english.rglob("*.md")}
    errors = []
    for name, target in translations.items():
        if name not in originals:
            errors.append(f"{name}: missing Chinese source")
            continue
        original = originals[name]
        digest = hashlib.sha256(original.read_bytes()).hexdigest()
        if reviewed.get(name) != digest:
            errors.append(f"{name}: translation source changed or review hash missing")
        old, new = original.read_text(), target.read_text()
        if [h[1] for h in headings(old)] != [h[1] for h in headings(new)]:
            errors.append(f"{name}: heading structure changed")
        if not code_blocks_match(old, new, root):
            errors.append(f"{name}: code blocks differ")
    for name in reviewed.keys() - translations.keys():
        errors.append(f"{name}: review record has no translation")
    return {"source_pages": len(originals), "translated_pages": len(translations),
            "missing": sorted(originals.keys() - translations.keys()), "errors": errors}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    args = parser.parse_args()
    result = inspect(ROOT)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return bool(result["errors"] or (args.require_complete and result["missing"]))


if __name__ == "__main__":
    raise SystemExit(main())
