#!/usr/bin/env python3
"""Upload a runner image archive to the offline-download share on Aliyun Drive (PDS).

The share is the folder that docs/how-to/deploy-offline.md links to; it holds
one directory per engine and framework (vllm-cuda/, sglang-dtk/, ...) with
files named gpuworks-runner_<tag>_linux-<arch>.tar.gz. The API key this uses
can upload and rename but cannot delete, so every upload is permanent: the
script refuses to overwrite a file that already exists, and --dry-run checks
the destination without writing anything.

Standard library only and Python 3.6 compatible, so it runs on the build host
as is. Credentials come from the environment (PDS_DOMAIN_ID, PDS_API_KEY), for
example `env $(grep -v '^#' pds-ent-apikey.env | xargs) ./pds-upload.py ...`.

    ./pds-upload.py gpuworks-runner_dtk26.04-vllm0.21.0-r1_linux-amd64.tar.gz vllm-dtk --dry-run
    ./pds-upload.py gpuworks-runner_dtk26.04-vllm0.21.0-r1_linux-amd64.tar.gz vllm-dtk
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

DRIVE_ID = "101"
SHARE_ROOT = "/gpuworks-images"
PART_BYTES = 256 << 20
NAME = re.compile(r"^gpuworks-runner_[A-Za-z0-9._-]+_linux-(amd64|arm64)\.tar\.gz$")


def api(domain, key, path, body):
    request = urllib.request.Request(
        "https://%s.api.aliyunfile.com%s" % (domain, path),
        data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            raw = response.read()
    except urllib.error.HTTPError as err:
        raise SystemExit("%s failed: HTTP %d %s" % (path, err.code, err.read().decode(errors="replace")[:300]))
    return json.loads(raw.decode()) if raw else {}


def find(domain, key, folder_id, name):
    # file/list returns no items for this share's folders; search by name does.
    query = 'parent_file_id = "%s" and name = "%s"' % (folder_id, name)
    return api(domain, key, "/v2/file/search", {"drive_id": DRIVE_ID, "query": query, "limit": 10}).get("items", [])


def put_part(url, data):
    for attempt in range(5):
        request = urllib.request.Request(url, data=data, method="PUT")
        try:
            with urllib.request.urlopen(request, timeout=600) as response:
                response.read()
            return
        except (urllib.error.URLError, OSError) as err:
            if attempt == 4:
                raise SystemExit("part upload failed: %s" % err)
            time.sleep(5 * (attempt + 1))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("archive", help="gpuworks-runner_<tag>_linux-<arch>.tar.gz")
    parser.add_argument("folder", help="share subdirectory, e.g. vllm-dtk")
    parser.add_argument("--dry-run", action="store_true", help="check the destination, upload nothing")
    args = parser.parse_args()

    name = os.path.basename(args.archive)
    if not NAME.match(name):
        raise SystemExit("archive name must be gpuworks-runner_<tag>_linux-<arch>.tar.gz, got %s" % name)
    domain, key = os.environ.get("PDS_DOMAIN_ID"), os.environ.get("PDS_API_KEY")
    if not domain or not key:
        raise SystemExit("PDS_DOMAIN_ID and PDS_API_KEY must be set")

    folder = api(domain, key, "/v2/file/get_by_path", {"drive_id": DRIVE_ID, "file_path": SHARE_ROOT + "/" + args.folder})
    if folder.get("type") != "folder":
        raise SystemExit("%s/%s is not a folder on the share" % (SHARE_ROOT, args.folder))
    if find(domain, key, folder["file_id"], name):
        raise SystemExit("%s/%s/%s already exists; uploads cannot be deleted, so it is left as is" % (SHARE_ROOT, args.folder, name))
    size = os.path.getsize(args.archive)
    parts = (size + PART_BYTES - 1) // PART_BYTES
    print("%s -> %s/%s (%d bytes, %d parts)" % (name, SHARE_ROOT, args.folder, size, parts))
    if args.dry_run:
        return

    created = api(domain, key, "/v2/file/create", {
        "drive_id": DRIVE_ID, "parent_file_id": folder["file_id"], "name": name, "type": "file",
        "check_name_mode": "refuse", "size": size,
        "part_info_list": [{"part_number": n} for n in range(1, parts + 1)],
    })
    file_id, upload_id = created["file_id"], created["upload_id"]
    urls = {p["part_number"]: p["upload_url"] for p in created["part_info_list"]}
    started = time.time()
    with open(args.archive, "rb") as source:
        for number in range(1, parts + 1):
            data = source.read(PART_BYTES)
            if time.time() - started > 1800:
                # Part URLs expire; refresh the rest before they do.
                fresh = api(domain, key, "/v2/file/get_upload_url", {
                    "drive_id": DRIVE_ID, "file_id": file_id, "upload_id": upload_id,
                    "part_info_list": [{"part_number": n} for n in range(number, parts + 1)],
                })
                urls.update({p["part_number"]: p["upload_url"] for p in fresh["part_info_list"]})
                started = time.time()
            put_part(urls[number], data)
            print("part %d/%d" % (number, parts), flush=True)
    done = api(domain, key, "/v2/file/complete", {"drive_id": DRIVE_ID, "file_id": file_id, "upload_id": upload_id})
    if done.get("size") != size:
        raise SystemExit("complete reported size %s, expected %d" % (done.get("size"), size))
    print("uploaded %s/%s/%s" % (SHARE_ROOT, args.folder, name))


if __name__ == "__main__":
    sys.exit(main())
