#!/usr/bin/env python3
"""Collect and apply audited weight-repository creation dates.

``apply`` is deliberately offline and reads only the committed seed plus a
developer-owned cache. ``collect`` is explicit developer-time network mode:
it disables ambient proxies, uses official APIs with timeout/retries/rate
limiting, and writes collision-resistant hashed cache entries. Product
startup never invokes this script or accesses the network.
"""
import argparse
import hashlib
import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import Request, ProxyHandler, build_opener

PROXY_VARIABLES = ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY", "all_proxy", "ALL_PROXY")
HF_API = "https://huggingface.co/api/models/"
MODELSCOPE_API = "https://modelscope.cn/api/v1/models/"
MODELERS_API = "https://modelers.cn/server/model/"


def cache_key(repo: str) -> str:
    return hashlib.sha256(repo.strip().lower().encode("utf-8")).hexdigest()


def parse_time(raw: str) -> str:
    value = raw.strip()
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def source_name(value: str) -> str:
    return "modelscope" if value.strip().lower() in ("modelsource", "modelscope") else value.strip().lower()


def collect_repositories(manifest):
    repos = {}
    for template in manifest.get("templates", []):
        for variant in template.get("variants", []):
            repo = variant.get("weight_model_id", "").strip()
            if repo:
                repos.setdefault(repo.lower(), (repo, source_name(variant.get("weight_source", ""))))
    return repos


def raw_repository_count(manifest):
    return len({v.get("weight_model_id", "").strip() for t in manifest.get("templates", []) for v in t.get("variants", []) if v.get("weight_model_id", "").strip()})


def repository_url(source: str, repo: str, hf_api: str, modelscope_api: str, modelers_api: str) -> str:
    bases = {"hf": hf_api, "modelscope": modelscope_api, "modelers": modelers_api}
    return bases[source].rstrip("/") + "/" + repo


def request_json(url: str, cache_path: Path, timeout: float, retries: int, rate_limit: float, opener):
    last_error = None
    for attempt in range(retries + 1):
        if attempt:
            time.sleep(min(30.0, rate_limit * (2 ** attempt)))
        try:
            request = Request(url, headers={"Accept": "application/json", "User-Agent": "gpuworks-template-release-collector/1"})
            with opener.open(request, timeout=timeout) as response:
                payload = response.read()
            cache_path.parent.mkdir(parents=True, exist_ok=True)
            cache_path.write_bytes(payload)
            if rate_limit:
                time.sleep(rate_limit)
            return True
        except (HTTPError, URLError, TimeoutError, OSError) as error:
            last_error = error
    print(f"collect failed url={url} error={type(last_error).__name__}")
    return False


def collect(args, manifest):
    for name in PROXY_VARIABLES:
        os.environ.pop(name, None)
    opener = build_opener(ProxyHandler({}))
    repos = collect_repositories(manifest)
    requests = 0
    for repo, source in repos.values():
        # ModelScope is the primary source for many imported rows, but the
        # apply path intentionally cross-checks the same ID on HF. Collect
        # both up front so an empty cache reproduces that fallback exactly.
        sources = [source]
        if source == "modelscope":
            if repo.lower().startswith("modelers_park/"):
                sources.append("modelers")
            sources.append("hf")
        for request_source in sources:
            path = args.cache_dir / request_source / (cache_key(repo) + ".json")
            if path.exists() and path.read_bytes().strip() and not args.refresh:
                continue
            requests += 1
            request_json(repository_url(request_source, repo, args.hf_api, args.modelscope_api, args.modelers_api), path, args.timeout, args.retries, args.rate_limit, opener)
    print(f"repositories={len(repos)} requests={requests} cache_dir={args.cache_dir}")


def read_result(repo, source, cache_dirs, hf_api, modelscope_api, modelers_api):
    path = cache_dirs[source] / (cache_key(repo) + ".json")
    url = repository_url(source, repo, hf_api, modelscope_api, modelers_api)
    if not path.exists() or not path.read_text().strip():
        return {"repo": repo, "source": source, "url": url, "note": "official_metadata_cache_missing"}
    try:
        data = json.loads(path.read_text())
    except json.JSONDecodeError:
        return {"repo": repo, "source": source, "url": url, "note": "official_metadata_cache_invalid"}
    if source == "modelscope":
        timestamp = data.get("Data", {}).get("CreatedTime", 0) if isinstance(data.get("Data"), dict) else 0
        field = "Data.CreatedTime"
        if not timestamp:
            reason = "repository_not_found_on_modelscope" if data.get("Code") == 10010205001 else "repository_metadata_unavailable_on_modelscope"
    elif source == "modelers":
        timestamp = data.get("data", {}).get("created_at", 0) if isinstance(data.get("data"), dict) else 0
        field = "created_at"
        if not timestamp:
            reason = "repository_metadata_unavailable_on_modelers"
    else:
        timestamp = data.get("createdAt", "")
        field = "createdAt"
        if not timestamp:
            reason = "repository_metadata_requires_authentication_or_is_private" if data.get("error") else "official_metadata_missing_created_at"
    if not timestamp:
        return {"repo": repo, "source": source, "url": url, "note": reason}
    try:
        release_date = (datetime.fromtimestamp(float(timestamp), timezone.utc).isoformat().replace("+00:00", "Z")
                        if source in ("modelscope", "modelers") else parse_time(str(timestamp)))
    except (TypeError, ValueError, OverflowError):
        return {"repo": repo, "source": source, "url": url, "note": "official_metadata_invalid_timestamp"}
    return {"repo": repo, "source": source, "url": url, "release_date": release_date, "metadata_field": field}


def evidence_url(manifest, repo):
    target = repo.strip().lower()
    for template in manifest.get("templates", []):
        for variant in template.get("variants", []):
            if variant.get("weight_model_id", "").strip().lower() != target:
                continue
            for support in variant.get("chip_supports", []):
                for evidence in support.get("evidence", []):
                    url = evidence.get("source_url", "").strip()
                    if url.startswith(("https://", "http://")):
                        return url
    return ""


def earliest_evidence_date(manifest, repo):
    target = repo.strip().lower()
    dates = []
    for template in manifest.get("templates", []):
        for variant in template.get("variants", []):
            if variant.get("weight_model_id", "").strip().lower() != target:
                continue
            for support in variant.get("chip_supports", []):
                for evidence in support.get("evidence", []):
                    captured = evidence.get("captured_at", "").strip()
                    if captured:
                        dates.append(parse_time(captured))
    return min(dates) if dates else ""


def apply(args, manifest):
    repos = collect_repositories(manifest)
    dirs = {"hf": args.hf_cache, "modelscope": args.modelscope_cache, "modelers": args.modelers_cache}
    results = []
    for repo, source in repos.values():
        result = read_result(repo, source, dirs, args.hf_api, args.modelscope_api, args.modelers_api)
        if "release_date" not in result and source == "modelscope":
            fallbacks = ["modelers", "hf"] if repo.lower().startswith("modelers_park/") else ["hf"]
            for fallback_source in fallbacks:
                fallback = read_result(repo, fallback_source, dirs, args.hf_api, args.modelscope_api, args.modelers_api)
                if "release_date" in fallback:
                    result = fallback
                    break
        special = "(" in repo or (repo.lower().endswith(".gguf") and "/" not in repo)
        if special:
            fixed = evidence_url(manifest, repo)
            if fixed:
                result["url"] = fixed
            if repo.lower().endswith(".gguf") and "/" not in repo:
                result["note"] = "unscoped_file_name_without_authoritative_repository"
        if "release_date" not in result and args.estimate_missing:
            estimated = earliest_evidence_date(manifest, repo)
            if estimated:
                missing_reason = result.get("note", "official_metadata_unavailable")
                result["release_date"] = estimated
                result["note"] = "estimated_from_earliest_upstream_evidence; " + missing_reason
        results.append(result)
    results.sort(key=lambda item: item["repo"].lower())
    by_repo = {item["repo"].lower(): item for item in results}
    for template in manifest.get("templates", []):
        for variant in template.get("variants", []):
            result = by_repo[variant["weight_model_id"].strip().lower()]
            for key in ("release_date", "release_date_source", "release_date_note"):
                variant.pop(key, None)
            variant["release_date_source"] = result["url"]
            if result.get("release_date"):
                variant["release_date"] = result["release_date"]
            else:
                variant["release_date_note"] = result["note"]
    fixture = {"captured_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
               "repository_count_case_sensitive": raw_repository_count(manifest),
               "repository_count_normalized": len(results), "results": results}
    args.fixture.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n")
    print(f"unique_repositories={len(results)} release_dates={sum('release_date' in r for r in results)} missing={sum('release_date' not in r for r in results)}")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("collect", "apply"))
    parser.add_argument("--seed", type=Path, required=True)
    parser.add_argument("--fixture", type=Path, required=True)
    parser.add_argument("--hf-cache", type=Path, required=True)
    parser.add_argument("--modelscope-cache", type=Path, required=True)
    parser.add_argument("--modelers-cache", type=Path)
    parser.add_argument("--cache-dir", type=Path, help="collect root; defaults to parent of --hf-cache")
    parser.add_argument("--hf-api", default=HF_API)
    parser.add_argument("--modelscope-api", default=MODELSCOPE_API)
    parser.add_argument("--modelers-api", default=MODELERS_API)
    parser.add_argument("--timeout", type=float, default=20.0)
    parser.add_argument("--retries", type=int, default=3)
    parser.add_argument("--rate-limit", type=float, default=0.2)
    parser.add_argument("--refresh", action="store_true")
    parser.add_argument("--estimate-missing", action="store_true", help="mark missing dates with an evidence-based estimate")
    args = parser.parse_args()
    args.modelers_cache = args.modelers_cache or args.hf_cache.parent / "modelers"
    manifest = json.loads(args.seed.read_text())
    if args.mode == "collect":
        args.cache_dir = args.cache_dir or args.hf_cache.parent
        collect(args, manifest)
    else:
        apply(args, manifest)


if __name__ == "__main__":
    main()
