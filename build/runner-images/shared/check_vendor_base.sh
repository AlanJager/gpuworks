#!/usr/bin/env bash
# 厂商 base 的「文件系统 + 历史」双重检查：确认它没有被夹带 ZStack/AIOS 内部组件。
#
# 为什么需要（2026-10-05 的教训）：
#   r2 的 base 清洗门只查 image history 里有没有凭据形态的字符串，**不查文件系统**。结果
#   dtk-vllm 的 base 其实是公司 Harbor 的 aiworker-dcu——ZStack 在海光镜像上叠了 AIOS 层
#   （zstack_ai_starter、juicefs、aios-*.sh、miniforge、CMD zstack_ai_starter）——一路通过了
#   清洗门，直到真机验证时才被 CMD 暴露。清 history ≠ base 干净。
#
# 做法：
#   1) history 里出现 ADD/COPY/RUN 带这些标记的条目 → 拒绝（见 MARKERS）；
#   2) 配置里 CMD/ENTRYPOINT 指向这些标记 → 拒绝；
#   3) 用 --fs 时，起一个只读临时容器扫文件系统里的标记文件 → 拒绝。
#
# 用法：check_vendor_base.sh <image-ref> [--fs]
#   --fs 需要本地 docker 能拉/跑该镜像（构建机的 daemon 拉不了明文 HTTP 的本地 mirror 时，
#   请在能跑它的机器上手动执行第 3 步，或用 build.sh 构建后对成品镜像再扫一次）。
# 退出码：0 干净，1 发现 ZStack/AIOS 痕迹，2 读不到。
set -uo pipefail
REF="${1:?usage: check_vendor_base.sh <image-ref> [--fs]}"; FS=0; [ "${2:-}" = "--fs" ] && FS=1
# 标记：文件名与指令里的关键词。aiosignal 是个常见 pip 包名，必须排除，别再误报。
MARKERS='zstack_ai_starter|zstack-ai-starter|juicefs|aios-config|aios_mount_daemon|/home/zstack|gitlab\.zstack|aiworker'
TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT
SKOPEO="$(command -v skopeo || echo /usr/local/bin/skopeo)"
if docker image inspect "$REF" >/dev/null 2>&1; then
    docker image inspect "$REF" --format '{{json .Config}}' > "$TMP"; docker history --no-trunc --format '{{.CreatedBy}}' "$REF" >> "$TMP" 2>/dev/null
else
    "$SKOPEO" inspect --config --tls-verify=false "docker://$REF" > "$TMP" 2>/dev/null || { echo "cannot read $REF" >&2; exit 2; }
fi
hits="$(grep -a -o -i -E "$MARKERS" "$TMP" | sort | uniq -c | sort -rn | head -8)"
if [ -n "$hits" ]; then
    echo "ZStack/AIOS markers found in config/history of $REF:"; echo "$hits" | sed 's/^/    /'; exit 1
fi
if [ $FS -eq 1 ]; then
    out="$(docker run --rm --entrypoint sh "$REF" -c 'for f in /root/zstack_ai_starter /usr/local/bin/juicefs /usr/local/bin/aios-config.sh /usr/local/bin/aios_mount_daemon.sh /home/zstack /root/run.sh; do [ -e "$f" ] && echo "$f"; done' 2>/dev/null)"
    if [ -n "$out" ]; then echo "ZStack/AIOS files present in $REF filesystem:"; echo "$out" | sed 's/^/    /'; exit 1; fi
fi
if [ $FS -eq 1 ]; then scope="history/config + filesystem"; else scope="history/config only; filesystem NOT scanned"; fi
echo "clean: no ZStack/AIOS markers in $REF ($scope)"
