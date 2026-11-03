"""Resolve explicitly selected unpublished ROCm entries in the runner matrix."""
import re


def select(matrix, image_id):
    rows = [row for row in matrix["items"] if row["id"] == image_id]
    if len(rows) != 1:
        raise ValueError(f"unknown or ambiguous runner matrix id: {image_id}")
    row = rows[0]
    if (row["backend"] != "rocm" or row.get("catalog") is not False
            or row["platforms"] != ["linux/amd64"]
            or row["engine"] not in ("vllm", "sglang")
            or not re.fullmatch(r"[^@]+@sha256:[0-9a-f]{64}", row["base"])):
        raise ValueError("local ROCm candidate requires an unpublished digest-pinned amd64 matrix entry")
    return row


def engine_version(row):
    return row["args"]["ENGINE_VERSION" if row["engine"] == "vllm" else "SGLANG_VERSION"]
