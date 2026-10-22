// Package installscript holds the agent installer shell script. It is a leaf:
// the server serves it, the SSH onboarding runs it on Linux and macOS, and
// the offline package runs it; none may hold a second copy of how a node
// records its configuration or registers its agent service.
package installscript

import (
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"
)

// Script is the agent installer, served as /install.sh for curl|sh. It puts
// the agent binary in place — the one GPUWORKS_AGENT_BINARY names (the SSH
// onboarding uploads it, the offline package unpacks it), else a download
// from --server — records the node's configuration in the state directory
// (gpuworks-agent configure, package agentconf), and starts
// `gpuworks-agent run --state DIR` under a systemd unit or nohup; on an Apple
// silicon Mac the binary registers its own launchd job (gpuworks-agent
// install-service, package launchd).
//
// The script is the same for every request. The server address and the
// per-node token come from the install command (--server, --token): the server
// never guesses the address a node should dial, and the token is never
// injected into a public script. When the server dials the agent
// (--connect-mode server_dial) there is no server address at all.
//
// --supervisor chooses what keeps a Linux agent running: systemd (the
// gpuworks-agent unit when systemd runs and the install may write it, else
// nohup), nohup (never touches the unit; several agents may run from their own
// directories), or keep (the way the host already runs this agent: nohup when
// it was a nohup install, else systemd).
var Script = strings.NewReplacer(
	"@MIN_MACOS@", strconv.Itoa(accelerator.MinimumMacOSMajor),
	"@HOME_INSTALL_DIR@", launchd.HomeInstallDir,
	"@CONF_FUNCS@", agentconf.ShellFuncs,
).Replace(script)

const script = `#!/bin/sh
set -eu

SERVER_URL=""
TOKEN=""
INSTALL_DIR="${GPUWORKS_INSTALL_DIR:-/opt/gpuworks}"
INSTALL_DIR_GIVEN="${GPUWORKS_INSTALL_DIR:+1}"
CONNECT_MODE="${GPUWORKS_CONNECT_MODE:-agent_dial}"
LISTEN_ADDR="${GPUWORKS_SERVER_DIAL_LISTEN:-}"
RUNTIME_DRIVER="${GPUWORKS_RUNTIME_DRIVER:-}"
RUNTIME_MOUNT_ROOT="${GPUWORKS_RUNTIME_MOUNT_ROOT:-}"
UDOCKER_BUNDLE="${GPUWORKS_UDOCKER_BUNDLE:-}"
SUPERVISOR=systemd
# Set GPUWORKS_NO_SYSTEMD=1 in hermetic installers/tests to force the
# supervisor-free fallback without touching the host's systemd unit.
if [ "${GPUWORKS_NO_SYSTEMD:-0}" = "1" ]; then SUPERVISOR=nohup; fi
# A nohup install runs the agent beside whatever the host already runs; only
# --clean-existing as well retires a gpuworks-agent unit an earlier install
# left.
CLEAN_EXISTING=""
STATE_DIR="${GPUWORKS_STATE_DIR:-}"
# --no-sudo installs with the caller's own rights although sudo works: the SSH
# onboarding passes it when sudo could not claim the install directory and it
# fell back to one the user can write.
NO_SUDO=""

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --server) SERVER_URL="$2"; shift 2 ;;
    --connect-mode) CONNECT_MODE="$2"; shift 2 ;;
    --listen) LISTEN_ADDR="$2"; shift 2 ;;
    --driver) RUNTIME_DRIVER="$2"; shift 2 ;;
    --mount-root) RUNTIME_MOUNT_ROOT="$2"; shift 2 ;;
    --udocker-bundle) UDOCKER_BUNDLE="$2"; shift 2 ;;
    --state) STATE_DIR="$2"; shift 2 ;;
    --install-dir) INSTALL_DIR="$2"; INSTALL_DIR_GIVEN=1; shift 2 ;;
    --supervisor) SUPERVISOR="$2"; shift 2 ;;
    --no-systemd) SUPERVISOR=nohup; shift ;;
    --clean-existing) CLEAN_EXISTING=1; shift ;;
    --no-sudo) NO_SUDO=1; shift ;;
    *) shift ;;
  esac
done
SERVER_URL="${SERVER_URL%/}"

STATE_FOLLOWS_INSTALL=""
if [ -z "$STATE_DIR" ]; then STATE_DIR="$INSTALL_DIR"; STATE_FOLLOWS_INSTALL=1; fi

if [ -z "$TOKEN" ]; then
  echo "gpuworks: --token is required; create a node in the UI to obtain an install command" >&2
  exit 2
fi
case "$CONNECT_MODE" in
  agent_dial)
    if [ -z "$SERVER_URL" ]; then
      echo "gpuworks: --server is required when the agent connects to the server; create a node in the UI to obtain an install command" >&2
      exit 2
    fi ;;
  server_dial) ;;
  *) echo "gpuworks: --connect-mode must be agent_dial or server_dial" >&2; exit 2 ;;
esac
case "$SUPERVISOR" in
  systemd|nohup|keep) ;;
  *) echo "gpuworks: --supervisor must be systemd, nohup or keep" >&2; exit 2 ;;
esac
if [ -z "${GPUWORKS_AGENT_BINARY:-}" ] && [ -z "$SERVER_URL" ]; then
  echo "gpuworks: no agent binary: give --server to download one, or run the offline package" >&2
  exit 2
fi

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
GOOS=linux
if [ "$(uname -s)" = "Darwin" ]; then
  GOOS=darwin
  if [ "$GOARCH" != "arm64" ]; then
    echo "gpuworks: macOS nodes need Apple silicon; engines use its Metal GPU" >&2
    exit 1
  fi
  MACOS_VERSION="$(sw_vers -productVersion 2>/dev/null || true)"
  if ! [ "${MACOS_VERSION%%.*}" -ge @MIN_MACOS@ ] 2>/dev/null; then
    echo "gpuworks: macOS nodes need macOS @MIN_MACOS@ or later; this Mac runs ${MACOS_VERSION:-an unknown version}" >&2
    exit 1
  fi
fi

if [ -n "${GPUWORKS_AGENT_BINARY:-}" ]; then
  echo "gpuworks: installing agent (${GOOS}/${GOARCH})"
else
  echo "gpuworks: installing agent from ${SERVER_URL} (${GOOS}/${GOARCH})"
fi
SUDO=""
if [ -n "$NO_SUDO" ]; then
  :
elif [ "$(id -u)" != "0" ] && command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  SUDO="sudo -n"
elif [ "$GOOS" = "darwin" ] && [ "$(id -u)" != "0" ] && command -v sudo >/dev/null 2>&1 && ( exec < /dev/tty ) 2>/dev/null; then
  # On a Mac the person installing is at the keyboard: sudo asks for the
  # password on the terminal even though this script arrives on stdin. The
  # terminal must open, not merely exist: a command run over SSH without one
  # (the onboarding wizard) has /dev/tty but no terminal behind it.
  echo "gpuworks: installing the agent as a launch daemon needs administrator rights"
  SUDO="sudo"
fi
if [ "$GOOS" = "darwin" ] && [ "$(id -u)" != "0" ] && [ -z "$SUDO" ] && [ -z "$INSTALL_DIR_GIVEN" ]; then
  # Without administrator rights /opt is not writable; the agent lives in the
  # user's home and runs as a launch agent of their session.
  INSTALL_DIR="$HOME/@HOME_INSTALL_DIR@"
  if [ -n "$STATE_FOLLOWS_INSTALL" ]; then STATE_DIR="$INSTALL_DIR"; fi
fi
$SUDO mkdir -p "$INSTALL_DIR" "$STATE_DIR"
if [ "$(id -u)" = "0" ] || [ -n "$SUDO" ]; then
  # A root-installed agent may execute udocker as a dedicated non-root user.
  # Keep private state files protected while allowing that user to traverse the
  # install root to the explicitly managed runtime-mounts directory.
  $SUDO chmod 0711 "$INSTALL_DIR"
fi
if [ -n "$SUDO" ]; then
  $SUDO chown "$(id -u):$(id -g)" "$INSTALL_DIR" "$STATE_DIR"
fi
BIN="$INSTALL_DIR/gpuworks-agent"

# Copy or download to a temporary file, then atomically replace the live
# binary. This avoids ETXTBSY on Linux and leaves the old agent executable if
# a download fails halfway through. A binary already uploaded into place is
# used as it is.
if [ "${GPUWORKS_AGENT_BINARY:-}" != "$BIN" ]; then
  TMP_BIN="${BIN}.tmp.$$"
  trap 'rm -f "$TMP_BIN"' EXIT
  if [ -n "${GPUWORKS_AGENT_BINARY:-}" ]; then
    cp "$GPUWORKS_AGENT_BINARY" "$TMP_BIN"
  else
    curl -fsSL "${SERVER_URL}/dl/gpuworks-agent-${GOOS}-${GOARCH}" -o "$TMP_BIN"
  fi
  chmod +x "$TMP_BIN"
  mv -f "$TMP_BIN" "$BIN"
  trap - EXIT
fi

if [ "$(id -u)" != "0" ] && [ -n "$SUDO" ] && [ "$RUNTIME_DRIVER" = "udocker" ]; then
  # The agent service retains its existing privileges; only the udocker
  # workload account receives the device groups it needs.
  $SUDO "$BIN" grant-device-access --state "$STATE_DIR" --user udocker --create-user
fi

@CONF_FUNCS@
unit_installed() {
  command -v systemctl >/dev/null 2>&1 && systemctl cat gpuworks-agent >/dev/null 2>&1
}
# The unit runs this node's agent when it runs with this node's token; a host
# may run other agents (a unit for another node, nohup ones from other
# directories) that an install or restart must leave alone.
own_unit() {
  unit_installed && [ "$(gw_unit_value TOKEN)" = "$TOKEN" ]
}

# Decide how the agent runs, and whose agent this replaces, before the
# configuration below overwrites the file those questions read.
USE_SYSTEMD=0
if [ "$GOOS" = "linux" ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ] && { [ "$(id -u)" = "0" ] || [ -n "$SUDO" ]; }; then
  case "$SUPERVISOR" in
    systemd) USE_SYSTEMD=1 ;;
    keep) if own_unit || { ! unit_installed && [ ! -e "$STATE_DIR/agent-restart.sh" ]; }; then USE_SYSTEMD=1; fi ;;
  esac
fi
OLD_SERVER=""
if [ "$USE_SYSTEMD" = 1 ] && unit_installed; then OLD_SERVER="$(gw_unit_value SERVER)"; fi
if [ -z "$OLD_SERVER" ]; then OLD_SERVER="$(gw_conf "$STATE_DIR" SERVER)"; fi
if [ -n "$OLD_SERVER" ] && [ "${OLD_SERVER%/}" != "$SERVER_URL" ]; then
  echo "gpuworks: WARNING: replacing the gpuworks agent installed for another server (${OLD_SERVER}); that server loses this host." >&2
fi

# The one record of how this agent connects; every start below runs
# "run --state". The token goes on stdin, not into an argument list ps shows.
set -- --state "$STATE_DIR" --connect-mode "$CONNECT_MODE" --listen "$LISTEN_ADDR" --driver "$RUNTIME_DRIVER" --mount-root "$RUNTIME_MOUNT_ROOT" --udocker-bundle "$UDOCKER_BUNDLE"
if [ -n "$SERVER_URL" ]; then set -- "$@" --server "$SERVER_URL"; fi
printf '%s\n' "$TOKEN" | "$BIN" configure "$@"

if [ "$GOOS" = "darwin" ]; then
  # launchd supervises the agent, and the agent registers its own job: with
  # administrator rights a launch daemon running as the installing user from boot,
  # without them a launch agent of the user's session.
  set -- --state "$STATE_DIR"
  if [ "$(id -u)" = "0" ] || [ -n "$SUDO" ]; then
    # The daemon runs as the person who installed it, never as root
    # (install-service refuses root): it starts engines and downloads models,
    # and the Metal GPU needs no privileges.
    set -- "$@" --user "${SUDO_USER:-$(id -un)}"
  fi
  $SUDO "$BIN" install-service "$@"
  echo "gpuworks: installed in $INSTALL_DIR"
  exit 0
fi

PID_FILE="$STATE_DIR/gpuworks-agent.pid"
# Stop the agent a previous nohup install started from this binary and state.
stop_nohup_agent() {
  [ -r "$PID_FILE" ] || return 0
  OLD_PID="$(cat "$PID_FILE" 2>/dev/null || true)"
  case "$OLD_PID" in
    ''|*[!0-9]*) ;;
    *)
      if [ -r "/proc/$OLD_PID/cmdline" ] &&
         [ "$(tr '\000' '\n' < "/proc/$OLD_PID/cmdline" | sed -n '1p')" = "$BIN" ] &&
         tr '\000' '\n' < "/proc/$OLD_PID/cmdline" | awk -v state="$STATE_DIR" 'previous == "--state" && $0 == state { found=1 } { previous=$0 } END { exit !found }'; then
        kill "$OLD_PID" 2>/dev/null || true
      fi
      ;;
  esac
}

if [ "$USE_SYSTEMD" = 1 ]; then
  # The unit supervises the agent from here on: retire a nohup one and its
  # restart script, which also marks the host as a nohup install.
  stop_nohup_agent
  rm -f "$PID_FILE" "$STATE_DIR/agent-restart.sh"
  cat <<GPUWORKS_AGENT_UNIT | $SUDO tee /etc/systemd/system/gpuworks-agent.service >/dev/null
[Unit]
Description=gpuworks agent
After=network-online.target
RequiresMountsFor=$INSTALL_DIR $STATE_DIR

[Service]
ExecStartPre=/usr/bin/test -x $BIN
ExecStart=$BIN run --state $STATE_DIR
# Keep inference containers alive when an agent reinstall restarts the service.
KillMode=process
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
GPUWORKS_AGENT_UNIT
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable gpuworks-agent
  $SUDO systemctl restart gpuworks-agent
  echo "gpuworks: agent started via systemd"
else
  echo "gpuworks: starting agent in background (nohup)"
  if [ -n "$CLEAN_EXISTING" ] && unit_installed && { [ "$(id -u)" = "0" ] || [ -n "$SUDO" ]; }; then
    # An earlier install's unit would start a second agent beside this one.
    # KillMode=process leaves inference containers running.
    echo "gpuworks: removing the gpuworks-agent systemd service an earlier install left."
    $SUDO systemctl disable --now gpuworks-agent >/dev/null 2>&1 || true
    $SUDO rm -f /etc/systemd/system/gpuworks-agent.service
    $SUDO systemctl daemon-reload >/dev/null 2>&1 || true
  fi
  stop_nohup_agent
  RESTART_SCRIPT="$STATE_DIR/agent-restart.sh"
  cat > "$RESTART_SCRIPT" <<GPUWORKS_AGENT_RESTART
#!/bin/sh
# Regenerated by every agent install. Hosts running the systemd unit restart
# through gpuworks-agent.service instead; this script is the crash and
# post-mortem restart path for nohup installs, where nothing supervises the
# agent. The agent reads its configuration from the state directory.
nohup "$BIN" run --state "$STATE_DIR" >/dev/null 2>&1 &
AGENT_PID=\$!
echo "\$AGENT_PID" > "$PID_FILE"
echo "restarted agent pid \$AGENT_PID"
GPUWORKS_AGENT_RESTART
  chmod 700 "$RESTART_SCRIPT"
  nohup "$BIN" run --state "$STATE_DIR" >/dev/null 2>&1 &
  AGENT_PID=$!
  printf '%s\n' "$AGENT_PID" > "$PID_FILE"
  echo "gpuworks: agent started via nohup pid $AGENT_PID (logs: $STATE_DIR/agent.log)"
fi
`
