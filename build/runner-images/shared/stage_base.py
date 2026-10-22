#!/usr/bin/env python3
"""把上游 base 在本地 mirror 里「注册」出来，不重新下载已有的层。

为什么需要：gpu3 到 docker.io 各镜像源实测只有 0.1~0.6 MB/s（2026-10-05，daocloud
与 1ms.run 都试过），一个 10 GB 的 cuda base 要拉 4 小时以上，而 r2 要重建 12 个档。

为什么可行：r1 的成品镜像就是从这些 base 构建的，所以 **base 的每一层都已经作为 blob
存在于本地 staging registry 里**（实测上游 base 的 37 层正是 r1 镜像层的前 37 个）。
镜像的层是内容寻址的，于是只要把 base 的 manifest 与 config（约 40 KB）补进本地
registry，registry 校验引用的 blob 都在之后就能直接服务这个 base——一个层都不用下载。

注意：registry 的 blob 是按仓库命名空间访问的——层虽然在本地，但挂在 r1 成品镜像所在的
仓库下。所以要用跨仓库 blob mount（POST .../blobs/uploads/?mount=<digest>&from=<repo>）
把已有 blob 链进目标仓库，这一步不传输数据。用 --mount-from 指定源仓库。

安全性：manifest 与 config 的字节原样搬运，digest 逐个校验；任何一层既不在目标仓库、
也 mount 不过来就报错退出（由调用方退回真正的 skopeo copy）。所以本地 base 与上游
base 是同一内容，不是"近似"。

用法：
    python3 stage_base.py <upstream-ref> <staging-ref> [--insecure]
    python3 stage_base.py vllm/vllm-openai:v0.30.0 \\
            172.25.17.156:5000/vllm/vllm-openai:v0.30.0 --insecure
可选 --via <mirror-host> 从镜像源取 manifest/config（内容同为上游，仅取小对象更快）。

实测（gpu3，2026-10-05）：daocloud 镜像源对 blob 的 token 鉴权会 403，而 docker.io
经 HTTP 代理可以正常取到 config。所以默认不传 --via，并在环境里设好 https_proxy。
"""
import argparse
import hashlib
import json
import os
import shutil
import sys
import tempfile
import time
import urllib.error
import urllib.request

INDEX_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}
MANIFEST_TYPES = {
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
}
ACCEPT = ", ".join(sorted(INDEX_TYPES | MANIFEST_TYPES))


def split_ref(ref):
    """<host>/<repo>:<tag> → (host, repo, tag)；无 host 段按 docker.io 处理。"""
    head, _, tail = ref.partition("/")
    if not tail or ("." not in head and ":" not in head and head != "localhost"):
        host, repo = "registry-1.docker.io", ref
        if "/" not in repo.split(":")[0]:
            repo = "library/" + repo
    else:
        host, repo = head, tail
    repo, sep, tag = repo.rpartition(":")
    if not sep or "/" in tag:
        repo, tag = (repo + sep + tag), "latest"
    return host, repo, tag


class Registry:
    def __init__(self, host, insecure=False):
        self.host = host
        self.scheme = "http" if insecure else "https"
        self.tokens = {}

    def _url(self, path):
        return "%s://%s%s" % (self.scheme, self.host, path)

    def request(self, method, path, repo, body=None, headers=None, expect=(200, 201, 202)):
        hdrs = dict(headers or {})
        token = self.tokens.get(repo)
        if token:
            hdrs["Authorization"] = "Bearer " + token
        req = urllib.request.Request(self._url(path), data=body, headers=hdrs, method=method)
        try:
            resp = urllib.request.urlopen(req)
        except urllib.error.HTTPError as exc:
            if exc.code == 401 and repo not in self.tokens:
                self._authenticate(exc, repo)
                return self.request(method, path, repo, body, headers, expect)
            if exc.code in expect:
                return exc
            raise
        return resp

    def _authenticate(self, exc, repo):
        challenge = exc.headers.get("WWW-Authenticate", "")
        if not challenge.lower().startswith("bearer "):
            raise RuntimeError("unsupported auth challenge: %r" % challenge)
        params = {}
        for part in challenge[7:].split(","):
            key, _, value = part.strip().partition("=")
            params[key] = value.strip('"')
        realm = params.pop("realm")
        params.setdefault("scope", "repository:%s:pull" % repo)
        query = "&".join("%s=%s" % (k, v) for k, v in params.items())
        with urllib.request.urlopen(realm + "?" + query) as resp:
            # 只能读一次：流读空后再 json.load 会抛错
            payload = json.load(resp)
        self.tokens[repo] = payload.get("token") or payload.get("access_token")

    def get_manifest(self, repo, reference):
        resp = self.request("GET", "/v2/%s/manifests/%s" % (repo, reference), repo,
                            headers={"Accept": ACCEPT})
        return resp.read(), resp.headers.get("Content-Type")

    def get_blob(self, repo, digest):
        path = "/v2/%s/blobs/%s" % (repo, digest)
        for attempt in (1, 2):
            hdrs = {}
            token = self.tokens.get(repo)
            if token:
                hdrs["Authorization"] = "Bearer " + token
            req = urllib.request.Request(self._url(path), headers=hdrs, method="GET")
            try:
                return _BLOB_OPENER.open(req).read()
            except urllib.error.HTTPError as exc:
                if exc.code == 401 and attempt == 1 and repo not in self.tokens:
                    self._authenticate(exc, repo)
                    continue
                raise
        raise RuntimeError("unreachable")

    def has_blob(self, repo, digest):
        try:
            self.request("HEAD", "/v2/%s/blobs/%s" % (repo, digest), repo, expect=(200,))
            return True
        except urllib.error.HTTPError:
            return False

    def mount_blob(self, repo, digest, from_repo):
        """跨仓库挂载已有 blob：成功返回 True（201），registry 不支持或 blob 不在源仓库
        时返回 False（会退化成 202 即「请上传」）。不传输数据。"""
        path = "/v2/%s/blobs/uploads/?mount=%s&from=%s" % (repo, digest, from_repo)
        try:
            resp = self.request("POST", path, repo, expect=(201, 202))
        except urllib.error.HTTPError:
            return False
        return getattr(resp, "status", getattr(resp, "code", None)) == 201

    def get_blob_to_file(self, repo, digest, attempts=3, expected=0, label=""):
        """把 blob 流式写到临时文件，边写边算 sha256。

        层动辄 1 GiB 以上，整块 read() 进内存既浪费又容易 OOM；这里只用固定大小的
        缓冲区，并在写完后校验 digest —— 校验不过就当这次下载失败，重试。
        """
        last = None
        for attempt in range(1, attempts + 1):
            handle, path = tempfile.mkstemp(prefix="stage-blob-")
            os.close(handle)
            try:
                hasher = hashlib.sha256()
                size = 0
                # 多 GB 的层要报进度：全程静默会让人以为卡死（实测补一个 2 GiB 的
                # 档要十几分钟，期间没有任何输出）。
                step = 128 * 1024 * 1024
                next_report = step
                started = time.time()
                with self._open_blob(repo, digest) as src, open(path, "wb") as dst:
                    while True:
                        chunk = src.read(1024 * 1024)
                        if not chunk:
                            break
                        hasher.update(chunk)
                        dst.write(chunk)
                        size += len(chunk)
                        if size >= next_report:
                            elapsed = max(time.time() - started, 0.001)
                            print("      %s %.0f/%.0f MiB (%.1f MiB/s)"
                                  % (label or digest[:19], size / 2**20,
                                     (expected or size) / 2**20, size / 2**20 / elapsed),
                                  flush=True)
                            next_report += step
                if "sha256:" + hasher.hexdigest() != digest:
                    raise RuntimeError("digest mismatch after download")
                return path, size
            except Exception as exc:  # 网络抖动/校验失败都重试
                os.unlink(path)
                last = exc
                if attempt < attempts:
                    print("    retry %d/%d for %s (%s)" % (attempt, attempts, digest[:19], exc))
                    time.sleep(3 * attempt)
        raise last

    def _open_blob(self, repo, digest):
        path = "/v2/%s/blobs/%s" % (repo, digest)
        for attempt in (1, 2):
            hdrs = {}
            token = self.tokens.get(repo)
            if token:
                hdrs["Authorization"] = "Bearer " + token
            req = urllib.request.Request(self._url(path), headers=hdrs, method="GET")
            try:
                return _BLOB_OPENER.open(req)
            except urllib.error.HTTPError as exc:
                if exc.code == 401 and attempt == 1 and repo not in self.tokens:
                    self._authenticate(exc, repo)
                    continue
                raise
        raise RuntimeError("unreachable")

    def put_blob_from_file(self, repo, digest, path, size):
        resp = self.request("POST", "/v2/%s/blobs/uploads/" % repo, repo, expect=(202,))
        location = resp.headers["Location"]
        if location.startswith("/"):
            location = self._url(location)
        sep = "&" if "?" in location else "?"
        with open(path, "rb") as body:
            req = urllib.request.Request(
                location + sep + "digest=" + digest, data=body,
                headers={"Content-Type": "application/octet-stream",
                         "Content-Length": str(size)},
                method="PUT")
            urllib.request.urlopen(req)

    def put_blob(self, repo, digest, data):
        resp = self.request("POST", "/v2/%s/blobs/uploads/" % repo, repo, expect=(202,))
        location = resp.headers["Location"]
        if location.startswith("/"):
            location = self._url(location)
        sep = "&" if "?" in location else "?"
        req = urllib.request.Request(location + sep + "digest=" + digest, data=data,
                                     headers={"Content-Type": "application/octet-stream"},
                                     method="PUT")
        urllib.request.urlopen(req)

    def put_manifest(self, repo, reference, data, content_type):
        req = urllib.request.Request(
            self._url("/v2/%s/manifests/%s" % (repo, reference)), data=data,
            headers={"Content-Type": content_type}, method="PUT")
        urllib.request.urlopen(req)


class _StripAuthOnRedirect(urllib.request.HTTPRedirectHandler):
    """重定向到 CDN 时去掉 Authorization 头。

    registry 的 blob GET 往往 302/307 到对象存储，而对象存储对带着 registry Bearer
    头的请求回 403（它自己用 URL 签名鉴权）。urllib 默认会把原始头一起带过去。
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None:
            for store in ("headers", "unredirected_hdrs"):
                current = getattr(new, store, None)
                if isinstance(current, dict):
                    setattr(new, store, {k: v for k, v in current.items()
                                         if k.lower() != "authorization"})
        return new


_BLOB_OPENER = urllib.request.build_opener(_StripAuthOnRedirect())


def digest_of(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("upstream")
    parser.add_argument("staging")
    parser.add_argument("--insecure", action="store_true", help="本地 mirror 走明文 HTTP")
    parser.add_argument("--via", default="", help="从该镜像源取 manifest/config（内容同上游）")
    parser.add_argument("--mount-from", default="",
                        help="已持有这些层的本地仓库（通常是 r1 成品镜像所在的仓库）")
    parser.add_argument("--max-fetch-bytes", type=int, default=8 * 1024 * 1024,
                        help="允许直接下载的缺失 blob 上限（默认 8 MiB，给 attestation 用）")
    args = parser.parse_args()

    up_host, up_repo, up_tag = split_ref(args.upstream)
    st_host, st_repo, st_tag = split_ref(args.staging)
    src = Registry(args.via or up_host)
    dst = Registry(st_host, insecure=args.insecure)

    raw, ctype = src.get_manifest(up_repo, up_tag)
    top = json.loads(raw)
    # index 里的**每一个**子 manifest 都要注册，包括 buildkit 的 attestation
    # （platform 为 unknown/unknown）。跳过它们会让 index 的引用不完整，registry
    # 会拒收整个 index（实测 lmsysorg/sglang 的 index PUT 返回 400）。
    # attestation 的 blob 只有几 KB，直接下载补齐即可。
    if top.get("mediaType") in INDEX_TYPES or "manifests" in top:
        children = [entry["digest"] for entry in top["manifests"]]
    else:
        children = [digest_of(raw)]

    staged = 0
    for child in children:
        if len(children) == 1 and children[0] == digest_of(raw):
            man_raw, man_ctype = raw, ctype
        else:
            man_raw, man_ctype = src.get_manifest(up_repo, child)
        assert digest_of(man_raw) == child or len(children) == 1, "manifest digest mismatch"
        manifest = json.loads(man_raw)
        if "layers" not in manifest:
            print("  skip %s (no layers; not an image manifest)" % child[:19])
            continue

        sizes = {l["digest"]: l.get("size", 0) for l in manifest["layers"]}
        missing = [d for d in sizes if not dst.has_blob(st_repo, d)]
        if missing and args.mount_from:
            mounted = [d for d in missing if dst.mount_blob(st_repo, d, args.mount_from)]
            if mounted:
                print("  mounted %d/%d layer blob(s) from %s (no data transfer)"
                      % (len(mounted), len(missing), args.mount_from))
            missing = [d for d in missing if d not in set(mounted)]
        # 剩下的只允许是小对象（attestation 之类）。大层一律拒绝——本脚本的前提就是
        # "不重新下载镜像层"，否则应该老老实实用 skopeo copy。
        oversized = [d for d in missing if sizes.get(d, 0) > args.max_fetch_bytes]
        if oversized:
            total = sum(sizes.get(d, 0) for d in missing)
            print("ABORT: %d of %d layer(s) missing, %d of them over the %d-byte limit; "
                  "missing total %.2f GiB (largest %.0f MiB)."
                  % (len(missing), len(sizes), len(oversized), args.max_fetch_bytes,
                     total / 2**30, max(sizes.get(d, 0) for d in oversized) / 2**20),
                  file=sys.stderr)
            print("       其余层已可挂载。若缺失总量可接受，用 --max-fetch-bytes 放宽让本脚本"
                  "补齐这几层；否则改用 skopeo copy 整体拉一次。", file=sys.stderr)
            return 1
        for index, digest in enumerate(missing, 1):
            expected = sizes.get(digest, 0)
            if expected > 32 * 1024 * 1024:
                print("    fetching blob %d/%d %s (%.0f MiB)"
                      % (index, len(missing), digest[:19], expected / 2**20), flush=True)
            path, size = src.get_blob_to_file(up_repo, digest, expected=expected,
                                              label="blob %d/%d" % (index, len(missing)))
            try:
                dst.put_blob_from_file(st_repo, digest, path, size)
            finally:
                os.unlink(path)
        if missing:
            total = sum(sizes.get(d, 0) for d in missing)
            print("  fetched %d blob(s) totalling %.2f MiB (rest were mounted)"
                  % (len(missing), total / 2**20))

        config = manifest["config"]
        if not dst.has_blob(st_repo, config["digest"]):
            blob = src.get_blob(up_repo, config["digest"])
            if digest_of(blob) != config["digest"]:
                print("ABORT: config blob digest mismatch", file=sys.stderr)
                return 1
            dst.put_blob(st_repo, config["digest"], blob)
            print("  staged config %s (%d bytes)" % (config["digest"][:19], len(blob)))
        dst.put_manifest(st_repo, digest_of(man_raw), man_raw, man_ctype or manifest.get("mediaType"))
        print("  staged manifest %s (%d layers, all blobs already present)"
              % (digest_of(man_raw)[:19], len(manifest["layers"])))
        staged += 1

    dst.put_manifest(st_repo, st_tag, raw, ctype or top.get("mediaType"))
    back, _ = dst.get_manifest(st_repo, st_tag)
    if digest_of(back) != digest_of(raw):
        print("ABORT: staged tag serves a different digest than upstream", file=sys.stderr)
        return 1
    print("OK %s -> %s  (%d manifest(s); digest %s preserved)"
          % (args.upstream, args.staging, staged, digest_of(raw)[:19]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
