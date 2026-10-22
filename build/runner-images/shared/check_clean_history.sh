#!/usr/bin/env bash
# 检查镜像的 config 与 history 里有没有凭据形态的字符串。
# 用于：prepare-base.sh 压完 history 后自检；build.sh 在用厂商 base 前把关；
#       verify.sh 对成品复验。退出码 0 = 干净，1 = 发现凭据，2 = 读不到。
#
# 本地 daemon 里有就用 docker 读；否则用 skopeo 直接读 registry（厂商 base 动辄
# 20-40 GB，没必要为了看 config 把它拉进 daemon；本地 staging 还是明文 HTTP，
# docker CLI 根本拉不动）。
#
# 环境变量：SKOPEO_TLS_VERIFY=false  —— 对明文 HTTP 的本地 registry 需要
set -uo pipefail
REF="${1:?usage: check_clean_history.sh <image-ref>}"
TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT

if docker image inspect "$REF" >/dev/null 2>&1; then
    # 只取 .Config：.ContainerConfig 在 Docker API >= 1.45 已移除，写进模板会让
    # 整条 inspect 失败（于是「检查不出凭据」被误当成「干净」——必须避免这种假阴性）。
    docker image inspect "$REF" --format '{{json .Config}}' > "$TMP" 2>/dev/null || { echo "cannot inspect $REF" >&2; exit 2; }
    docker history --no-trunc --format '{{.CreatedBy}}' "$REF" >> "$TMP" 2>/dev/null || true
    src="docker-daemon"
else
    SKOPEO="$(command -v skopeo || echo /usr/local/bin/skopeo)"
    [ -x "$SKOPEO" ] || { echo "cannot read $REF: not in daemon and no skopeo" >&2; exit 2; }
    flags=()
    [ "${SKOPEO_TLS_VERIFY:-true}" = "false" ] && flags+=(--tls-verify=false)
    # --config 返回 image config blob，里面同时含 .config 与 .history
    if "$SKOPEO" inspect --config "${flags[@]+"${flags[@]}"}" "docker://$REF" > "$TMP" 2>/dev/null; then
        src="registry"
    elif "$SKOPEO" inspect --config --tls-verify=false "docker://$REF" > "$TMP" 2>/dev/null; then
        # 内网 mirror 常是明文 HTTP。这里只是读 config 找凭据，不据此信任镜像内容，
        # 所以退回明文是可接受的；真正的内容信任来自 base 的 digest。
        src="registry(plain-http)"
    else
        echo "cannot read $REF via skopeo" >&2; exit 2
    fi
fi

python3 - "$TMP" "$src" <<'PY'
import re, sys
text = open(sys.argv[1], errors="replace").read()
hits = re.findall(r'[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@"\']{1,64}:[^/\s@"\']{1,128}@[A-Za-z0-9.-]+', text)
if hits:
    hosts = sorted({h.rsplit('@', 1)[1] for h in hits})
    print("credential-shaped URLs found: %d (via %s; host(s): %s)"
          % (len(hits), sys.argv[2], ", ".join(hosts)))
    sys.exit(1)
print("clean: no credential-shaped URLs in config/history (via %s)" % sys.argv[2])
PY
