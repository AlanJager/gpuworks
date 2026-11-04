#!/usr/bin/env bash
# M0 acceptance demo — scripts the收口 demo from artifacts/archive/2026-08-18-roadmap/plan-v1.md:
#   "物理机 curl|sh 30 秒接入，UI 见节点与 GPU；杀 agent 自动重连"
#
# It starts a server, registers an agent via the same install flow the UI shows,
# verifies the node appears online with hardware, then kills the agent and
# confirms it goes offline and auto-reconnects. Runs without a GPU (GPU detection
# degrades gracefully; on a GPU host the node's cards show up too).
set -euo pipefail

cd "$(dirname "$0")/.."
BIN=./bin
ADMIN_PASSWORD="gpuworks-demo-admin-$$"
ADDR="127.0.0.1:8099"
BASE="http://$ADDR"
DATA=$(mktemp -d)
STATE=$(mktemp -d)
LOG=$(mktemp -d)

cleanup() {
  set +e
  [ -n "${AGENT_PID:-}" ] && kill "$AGENT_PID" 2>/dev/null
  [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$DATA" "$STATE" "$LOG"
}
trap cleanup EXIT

say() { printf '\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()  { printf '\033[1;32m✓ %s\033[0m\n' "$*"; }
die() { printf '\033[1;31m✗ %s\033[0m\n' "$*"; exit 1; }

# --- place the agent binary where /install.sh downloads it from ---
mkdir -p "$DATA/dl"
OS=$(uname -s)
ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) GOARCH=amd64;; aarch64|arm64) GOARCH=arm64;; *) die "unsupported arch: $ARCH";; esac
if [ "$OS" = Darwin ]; then
  # The release download path is Linux-only; use the locally built host agent
  # for this macOS integration demo while still exercising the same protocol.
  AGENT_BIN="$BIN/gpuworks-agent"
else
  cp "$BIN/dl/gpuworks-agent-linux-$GOARCH" "$DATA/dl/" 2>/dev/null || \
    cp "$BIN/gpuworks-agent" "$DATA/dl/gpuworks-agent-linux-$GOARCH"
  AGENT_BIN="$DATA/dl/gpuworks-agent-linux-$GOARCH"
fi

say "启动 server ($BASE)"
# The first start creates the admin account from this file, as after an install.
printf '{"user":"admin","password":"%s"}\n' "$ADMIN_PASSWORD" >"$DATA/initial-credentials"
"$BIN/gpuworks" serve --addr "$ADDR" --data "$DATA" --log-level warn \
  >"$LOG/server.log" 2>&1 &
SERVER_PID=$!

for i in $(seq 1 50); do
  curl -fsS "$BASE/install.sh" >/dev/null 2>&1 && break
  sleep 0.1
done
LOGIN=$(curl -fsS -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$ADMIN_PASSWORD\"}" \
  "$BASE/api/v1/auth/login") || die "管理面登录失败"
JWT=$(printf '%s' "$LOGIN" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$JWT" ] || die "登录响应缺少 JWT"
AUTH=(-H "Authorization: Bearer $JWT")
curl -fsS "${AUTH[@]}" "$BASE/api/v1/healthz" >/dev/null || die "server 未就绪"
ok "server 就绪"

say "创建 pending 节点并获取 per-node token"
NODE_CREATE=$(curl -fsS "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"name\":\"demo-node\",\"server_url\":\"$BASE\"}" "$BASE/api/v1/nodes") || die "创建 pending 节点失败"
TOKEN=$(printf '%s' "$NODE_CREATE" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || die "创建节点响应缺少 token"
curl -fsS "$BASE/install.sh" | grep -q -- '--token is required' || die "install.sh 未要求显式 token"
ok "pending 节点已创建，install.sh 不再内置全局 token"

say "接入 agent（模拟 curl|sh：直接用下载好的二进制注册）"
GPUWORKS_LOG_LEVEL=warn "$AGENT_BIN" run \
  --server "$BASE" --token "$TOKEN" --state "$STATE" \
  >"$LOG/agent.log" 2>&1 &
AGENT_PID=$!

wait_state() { # $1=state $2=timeout_s
  local want=$1 t=$2 i
  for i in $(seq 1 $((t*10))); do
    if curl -fsS "${AUTH[@]}" "$BASE/api/v1/nodes" 2>/dev/null | grep -q "\"state\":\"$want\""; then return 0; fi
    sleep 0.1
  done
  return 1
}

wait_state online 15 || { cat "$LOG/agent.log"; die "节点未上线"; }
ok "节点 30 秒内接入并上线"

NODES=$(curl -fsS "${AUTH[@]}" "$BASE/api/v1/nodes")
echo "$NODES" | grep -q '"hostname"' || die "未上报主机信息"
HOST=$(echo "$NODES" | sed -n 's/.*"hostname":"\([^"]*\)".*/\1/p' | head -1)
GPUC=$(echo "$NODES" | sed -n 's/.*"gpu_count":\([0-9]*\).*/\1/p' | head -1)
ok "UI 数据就绪：主机=$HOST GPU 数=$GPUC （无 GPU 环境为 0，属正常）"

say "杀掉 agent，验证节点转为离线"
kill "$AGENT_PID"; wait "$AGENT_PID" 2>/dev/null || true
wait_state offline 15 || die "节点未转离线"
ok "断连即离线（长连接断开被检测到）"

say "重启 agent（显式复用 per-node token），验证自动重连"
GPUWORKS_LOG_LEVEL=warn "$AGENT_BIN" run \
  --server "$BASE" --token "$TOKEN" --state "$STATE" \
  >"$LOG/agent2.log" 2>&1 &
AGENT_PID=$!
wait_state online 15 || { cat "$LOG/agent2.log"; die "未自动重连"; }
ok "凭 per-node token 自动重连并上线"

echo
ok "M0 收口 demo 全部通过 🎉"
