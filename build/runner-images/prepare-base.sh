#!/usr/bin/env bash
# 把一个厂商 base 的 image history 压掉，产出可安全分发的干净 base。
#
# 为什么需要：厂商 vanilla（asllm 等）的 image config history 里含厂商内网代理的
# 明文账号密码，而 history 随镜像分发到每个 registry —— 任何以它为 base 的镜像
# 都会把这段 history 带上。
#
# 为什么在 base 这一层做、而不是对最终镜像做（2026-10-05 实测）：
#   hggc 的两个 tag 共享 asllm vanilla 的全部层，staging 里 sum 50.4 GB / union
#   25.4 GB（省 50%）。若对每个最终镜像单独 squash，各自变成一个 ~25 GB 的独立
#   单层，去重消失，registry 存储和推送量都翻倍。在 base 上压一次，两个最终镜像
#   继续共享它，各自只叠自己的薄层。
#
# 做法派生自 gpustack/runner 的 pack/squash_image.sh（Apache-2.0，见 NOTICE）。
#
# 用法：
#   ./prepare-base.sh <src-ref> <dst-ref> [platform]
#   ./prepare-base.sh 192.0.2.156:5000/asllm:2.0.0-vanilla \
#                     192.0.2.156:5000/asllm:2.0.0-vanilla-clean
# 之后把 base-mirrors.local 里的映射指向 dst-ref。
set -euo pipefail

SRC="${1:?usage: prepare-base.sh <src-ref> <dst-ref> [platform]}"
DST="${2:?usage: prepare-base.sh <src-ref> <dst-ref> [platform]}"
PLATFORM="${3:-linux/amd64}"
BUILDER="${BUILDER:-gpuworks-builder}"

echo "==> checking that $SRC is a vendor original (before its history is squashed away)"
if ! SKOPEO_TLS_VERIFY="${SKOPEO_TLS_VERIFY:-true}" bash "$(dirname "$0")/shared/check_vendor_base.sh" "$SRC"; then
    echo "ERROR: $SRC carries ZStack/AIOS components; squashing its history would only hide that. Use the vendor original." >&2
    exit 1
fi
echo "==> squashing history: $SRC -> $DST ($PLATFORM)"

# 用 skopeo 读源镜像的 config：厂商 base 有 20-40 GB，没必要为了读 config 把它
# 拉进 daemon；本地 staging registry 还是明文 HTTP，docker CLI 也拉不动。
# buildkit 自己会按需拉取源镜像（它的 insecure registry 配置里有 staging）。
SKOPEO="$(command -v skopeo || echo /usr/local/bin/skopeo)"
[ -x "$SKOPEO" ] || { echo "skopeo required" >&2; exit 1; }
sflags=()
[ "${SKOPEO_TLS_VERIFY:-true}" = "false" ] && sflags+=(--tls-verify=false)
CONFIG="$("$SKOPEO" inspect --config "${sflags[@]+"${sflags[@]}"}" "docker://$SRC" \
    | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin).get("config", {})))')"
[ -n "$CONFIG" ] && [ "$CONFIG" != "{}" ] || { echo "ERROR: empty config read from $SRC" >&2; exit 1; }
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
{
  echo "FROM scratch"
  echo "COPY --from=${SRC} / /"
  python3 - "$CONFIG" <<'PY'
import json, sys
c = json.loads(sys.argv[1])
labels = {k: v for k, v in (c.get("Labels") or {}).items()
          if k not in ("maintainer", "org.opencontainers.image.created",
                       "org.opencontainers.image.url", "org.opencontainers.image.source")}
if labels:
    print("LABEL " + " ".join('%s="%s"' % (k, str(v).replace('"', '\\"')) for k, v in labels.items()))
for env in (c.get("Env") or []):
    k, _, v = env.partition("=")
    print('ENV %s="%s"' % (k, v.replace('"', '\\"')))
if c.get("User"):       print("USER " + c["User"])
if c.get("WorkingDir"): print("WORKDIR " + c["WorkingDir"])
for vol in sorted((c.get("Volumes") or {}).keys()):
    print("VOLUME " + vol)
if c.get("Entrypoint"): print("ENTRYPOINT " + json.dumps(c["Entrypoint"]))
if c.get("Cmd"):        print("CMD " + json.dumps(c["Cmd"]))
PY
} > "$WORK/Dockerfile"

echo "--- generated Dockerfile:"; sed 's/^/    /' "$WORK/Dockerfile"
docker buildx build --builder "$BUILDER" --platform "$PLATFORM" \
    --provenance=false --sbom=false \
    --file "$WORK/Dockerfile" --tag "$DST" --push "$WORK"

echo "==> verifying the result carries no credential-shaped history"
if ! bash "$(dirname "$0")/shared/check_clean_history.sh" "$DST"; then
    echo "ERROR: $DST still contains credential-shaped strings in its config/history" >&2
    exit 1
fi
echo "==> done: $DST （把 base-mirrors.local 的映射指向它）"
