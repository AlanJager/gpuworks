#!/usr/bin/env bash
# 严格应用补丁，并把结果写成清单供 verify 对账。
#
# 用法：PATCH_DIR=/tmp/patches-vllm TARGET=<site-packages> \
#         MANIFEST=/opt/gpuworks/patches.json [EXPECT_ALL=1] [NOT_APPLICABLE="a.patch b.patch"] \
#         bash apply_patches.sh
#
# 为什么不用 `patch --fuzz=N ... || echo WARN`：
#   本仓补丁的 hunk 只有 3 行上下文，fuzz>=3 等于不校验上下文。实测 GNU patch
#   2.7.6 在上下文完全不匹配时仍按原行号插入并返回 0（"succeeded with fuzz 3"），
#   把纯追加型补丁插进类方法中间，文件直接 IndentationError；而这类坏掉的模块
#   （例如 mooncake connector）只在 PD 部署时才被 import，平时发现不了。
#
# 规则：
#   1) 先 --dry-run 试算，不通过就不落盘；
#   2) 不给 --fuzz（默认 fuzz=2 仍嫌宽，显式 -F0 要求上下文精确匹配）；
#   3) EXPECT_ALL=1（默认）时，任一补丁不适用即整体失败；
#      EXPECT_ALL=0 时允许「上游已修」的补丁被跳过，但必须记进清单；
#   4) NOT_APPLICABLE（空格分隔的补丁文件名）：矩阵显式声明「本档的 base 上该补丁不适用」。
#      这样 EXPECT_ALL=1 仍可保持严格：声明过的补丁记进清单 skipped（`名:declared-not-applicable`），
#      没声明的不适用补丁照样失败；反过来，声明了不适用、实际却能应用（上游又变了/声明过期），
#      同样失败——声明必须与事实一致，不会悄悄漂移。
#   5) 不留 .orig/.rej（-r /dev/null + --no-backup-if-mismatch）。
set -euo pipefail

PATCH_DIR="${PATCH_DIR:?set PATCH_DIR}"
TARGET="${TARGET:?set TARGET}"
MANIFEST="${MANIFEST:-}"
EXPECT_ALL="${EXPECT_ALL:-1}"
NOT_APPLICABLE=" ${NOT_APPLICABLE:-} "

shopt -s nullglob
patches=("${PATCH_DIR}"/*.patch "${PATCH_DIR}"/*.diff)
shopt -u nullglob

if [ ${#patches[@]} -eq 0 ]; then
    echo "apply_patches: no patches in ${PATCH_DIR}, nothing to do"
    [ -n "${MANIFEST}" ] && echo '{"applied":[],"skipped":[]}' > "${MANIFEST}"
    exit 0
fi

applied=()
skipped=()
failed=()

for p in "${patches[@]}"; do
    name="$(basename "${p}")"
    if [[ "${NOT_APPLICABLE}" == *" ${name} "* ]]; then
        if patch -p1 -F0 --dry-run --forward -d "${TARGET}" < "${p}" >/dev/null 2>&1 \
            || patch -p1 -F0 --dry-run --reverse -d "${TARGET}" < "${p}" >/dev/null 2>&1; then
            echo "apply_patches: ${name} is declared NOT_APPLICABLE but applies (or is already present): the declaration is stale, drop it" >&2
            failed+=("${name}")
        else
            skipped+=("${name}:declared-not-applicable")
            echo "apply_patches: skipped ${name} (declared not applicable to this base)"
        fi
        continue
    fi
    if patch -p1 -F0 --dry-run --forward -d "${TARGET}" < "${p}" >/dev/null 2>&1; then
        patch -p1 -F0 --forward --no-backup-if-mismatch -r /dev/null \
            -d "${TARGET}" < "${p}" >/dev/null
        applied+=("${name}")
        echo "apply_patches: applied ${name}"
    elif patch -p1 -F0 --dry-run --reverse -d "${TARGET}" < "${p}" >/dev/null 2>&1; then
        # 反向可应用 = 上游已经包含该修复，跳过是正确的。
        skipped+=("${name}:already-present")
        echo "apply_patches: skipped ${name} (already present upstream)"
    else
        failed+=("${name}")
        echo "apply_patches: DOES NOT APPLY ${name}" >&2
    fi
done

if [ -n "${MANIFEST}" ]; then
    mkdir -p "$(dirname "${MANIFEST}")"
    python3 - "${MANIFEST}" <<PY
import json, sys
json.dump({
    "applied": "${applied[*]-}".split(),
    "skipped": "${skipped[*]-}".split(),
    "failed": "${failed[*]-}".split(),
}, open(sys.argv[1], "w"), indent=2, sort_keys=True)
PY
fi

if [ ${#failed[@]} -gt 0 ]; then
    if [ "${EXPECT_ALL}" = "1" ]; then
        echo "=== apply_patches FAILED: ${#failed[@]} patch(es) do not apply to this base." >&2
        echo "    Upstream drifted. Rebase the patch, or declare it in NOT_APPLICABLE (matrix.yaml" >&2
        echo "    args, with a note) if this base genuinely does not need it —— 不要靠 fuzz 糊过去。" >&2
        exit 1
    fi
    echo "apply_patches: ${#failed[@]} patch(es) skipped under EXPECT_ALL=0" >&2
fi
echo "apply_patches: applied=${#applied[@]} skipped=${#skipped[@]} failed=${#failed[@]}"
