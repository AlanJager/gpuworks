//go:build cloudgpu && partition

package l4

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPPUAgentProbeCapabilityEndToEnd locks in the regression that motivated
// the PPU probe correction: host HGGC=N/A must not pre-empt a real Docker/runc
// probe. It starts an isolated current server and agent on the PPU host, then
// verifies the wire report, persisted probe, derived API capability, embedded
// image loading, and cleanup. No test-host address or credential is archived.
func TestPPUAgentProbeCapabilityEndToEnd(t *testing.T) {
	host := requiredHostEnv(t, "GPUWORKS_PPU_HOST")
	jump := hostEnv("GPUWORKS_PPU_SSH_JUMP", "gpu3")
	runID := "gpuworks-ppu-agent-probe-l4-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteDir := "/tmp/" + runID

	preflightCtx, preflightCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer preflightCancel()
	preflight := `set -eu
test "$(uname -m)" = x86_64
command -v docker >/dev/null
command -v python3 >/dev/null
command -v ppu-smi >/dev/null
test "$(docker info --format '{{.DefaultRuntime}}')" = runc
ppu-smi --query-ppu=index,name,architecture,uuid --format=csv,noheader,nounits | grep -q 'PPU-ZW810E'
test "$(ppu-smi -q -d VERSION | awk -F: '/HGGC Runtime Version/{gsub(/^[ \t]+|[ \t]+$/, "", $2); print $2; exit}')" = N/A`
	if out, err := partitionSSHCommand(preflightCtx, host, jump, preflight).CombinedOutput(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("PPU automatic-probe preflight: %v: %s", err, out)
		}
		skipOrFailExternal(t, "PPU automatic-probe fixture unavailable: %v", err)
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repository path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	localBin := t.TempDir()
	buildCtx, buildCancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-trimpath", "-o", localBin, "./cmd/gpuworks", "./cmd/gpuworks-agent")
	build.Dir = repo
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build current PPU L4 server and agent: %v\n%s", err, out)
	}
	serverBin := filepath.Join(localBin, "gpuworks")
	agentBin := filepath.Join(localBin, "gpuworks-agent")

	prepareCtx, prepareCancel := context.WithTimeout(t.Context(), time.Minute)
	defer prepareCancel()
	prepare := "set -eu; mkdir " + shellQuoteL4(remoteDir) + "; chmod 700 " + shellQuoteL4(remoteDir)
	if out, err := partitionSSHCommand(prepareCtx, host, jump, prepare).CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated PPU probe directory: %v: %s", err, out)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		cleanup := fmt.Sprintf(`set +e
dir=%s
for role in agent server; do
  pidfile="$dir/$role.pid"
  test -s "$pidfile" || continue
  pid=$(cat "$pidfile")
  exe=$(readlink -f "/proc/$pid/exe" 2>/dev/null)
  case "$exe" in "$dir/gpuworks"|"$dir/gpuworks-agent") kill "$pid" 2>/dev/null ;; esac
done
for attempt in $(seq 1 20); do
  alive=0
  for role in agent server; do
    test -s "$dir/$role.pid" || continue
    kill -0 "$(cat "$dir/$role.pid")" 2>/dev/null && alive=1
  done
  test "$alive" = 0 && break
  sleep 0.2
done
case "$dir" in /tmp/gpuworks-ppu-agent-probe-l4-*) rm -rf -- "$dir" ;; *) exit 2 ;; esac`, shellQuoteL4(remoteDir))
		if out, err := partitionSSHCommand(cleanupCtx, host, jump, cleanup).CombinedOutput(); err != nil {
			t.Errorf("cleanup isolated PPU probe server/agent: %v: %s", err, out)
		}
	})

	copyCtx, copyCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer copyCancel()
	if out, err := partitionSCPCommand(copyCtx, jump, []string{serverBin, agentBin}, host+":"+remoteDir+"/").CombinedOutput(); err != nil {
		t.Fatalf("copy current PPU probe binaries: %v: %s", err, out)
	}

	portCtx, portCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer portCancel()
	portOut, err := partitionSSHCommand(portCtx, host, jump, `python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'`).CombinedOutput()
	if err != nil {
		t.Fatalf("allocate isolated PPU control-plane port: %v: %s", err, portOut)
	}
	portText := ""
	for _, line := range strings.Split(string(portOut), "\n") {
		line = strings.TrimSpace(line)
		if _, parseErr := strconv.Atoi(line); parseErr == nil {
			portText = line
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1024 || port > 65535 {
		t.Fatalf("invalid isolated PPU port %q", portOut)
	}

	adminPassword := randomL4Secret(t)
	masterSecret := randomL4Secret(t)
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	remoteRun := fmt.Sprintf(`set -eu
dir=%s
server_url=%s
if docker image inspect gpuworks/partition-probe:v3 >/dev/null 2>&1; then
  docker image rm gpuworks/partition-probe:v3 >/dev/null
  image_precondition=removed
else
  image_precondition=absent
fi
mkdir -p "$dir/data"
(umask 077; printf '{"user":"admin","password":"%%s"}\n' %s >"$dir/data/initial-credentials"; printf '%%s' %s >"$dir/data/master.key")
nohup "$dir/gpuworks" serve --addr "127.0.0.1:%d" --data "$dir/data" >"$dir/server.log" 2>&1 &
echo $! >"$dir/server.pid"
for attempt in $(seq 1 60); do
  if curl -fsS -H 'Content-Type: application/json' \
    -d %s "$server_url/api/v1/auth/login" >"$dir/login.json"; then break; fi
  sleep 0.25
done
auth_token=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token"])' "$dir/login.json")
curl -fsS -H "Authorization: Bearer $auth_token" -H 'Content-Type: application/json' \
  -d '{"name":"PPU automatic probe L4","connect_mode":"agent_dial","server_url":"%s"}' \
  "$server_url/api/v1/nodes" >"$dir/enroll.json"
node_token=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token"])' "$dir/enroll.json")
node_uuid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["node"]["uuid"])' "$dir/enroll.json")
nohup "$dir/gpuworks-agent" run --server "$server_url" --token "$node_token" \
  --state "$dir/agent-state" --driver docker >"$dir/agent.stdout" 2>&1 &
echo $! >"$dir/agent.pid"
rm -f "$dir/login.json" "$dir/enroll.json"
ready=0
for attempt in $(seq 1 120); do
  curl -fsS -H "Authorization: Bearer $auth_token" \
    "$server_url/api/v1/nodes/$node_uuid" >"$dir/node.json" || true
  if NODE_JSON="$dir/node.json" python3 - <<'PY'
import json, os
try:
    node = json.load(open(os.environ["NODE_JSON"]))
except Exception:
    raise SystemExit(1)
caps = [c for c in node.get("partition_capabilities", []) if c.get("backend") == "ppu-dgpu"]
if node.get("state") != "online" or not caps:
    raise SystemExit(1)
cap = caps[0]
if cap.get("status") != "ready" or cap.get("probe_status") != "passed" or not cap.get("probe_checked_at"):
    raise SystemExit(1)
PY
  then ready=1; break; fi
  sleep 0.5
done
test "$ready" = 1
test "$image_precondition" = absent -o "$image_precondition" = removed
docker image inspect gpuworks/partition-probe:v3 >/dev/null
test -z "$(docker ps -a --format '{{.Names}}' | grep '^gpuworks-partition-probe-ppu-dgpu-' || true)"
test -z "$(find "$dir/agent-state/dgpu-runtime/instances" -mindepth 1 -maxdepth 1 -name 'probe-ppu-dgpu-*' -print 2>/dev/null || true)"
test -z "$(find /dev/shm -mindepth 1 -maxdepth 1 -name 'probe-ppu-dgpu-*' -print 2>/dev/null || true)"
NODE_JSON="$dir/node.json" DB="$dir/data/gpuworks.db" IMAGE_PRECONDITION="$image_precondition" python3 - <<'PY'
import json, os, sqlite3
node = json.load(open(os.environ["NODE_JSON"]))
assert node.get("gpu_vendor") == "ppu", node
assert node.get("framework") == "hggc", node
assert node.get("framework_version", "") == "", node
assert node.get("runtime_driver") == "docker", node
assert "docker" in node.get("runtimes", []), node
caps = [c for c in node["partition_capabilities"] if c["backend"] == "ppu-dgpu"]
assert len(caps) == 1, caps
cap = caps[0]
assert cap["status"] == "ready", cap
assert cap["probe_status"] == "passed", cap
assert cap.get("probe_checked_at"), cap
db = sqlite3.connect(os.environ["DB"])
raw = db.execute("select partition_probes from nodes").fetchone()[0]
probes = [p for p in json.loads(raw) if p["backend"] == "ppu-dgpu"]
assert len(probes) == 1, probes
probe = probes[0]
assert probe["status"] == "passed", probe
assert "count=1" in probe["detail"], probe
assert "total_bytes=4294967296" in probe["detail"], probe
assert "quota_mib=4096" in probe["detail"], probe
print("host_hggc_runtime=N/A")
print("framework=hggc version=<empty>")
print("runtime_driver=docker runtimes=docker")
print("probe_image_precondition=" + os.environ["IMAGE_PRECONDITION"])
print("probe_image_after=loaded")
print("probe_status=passed")
print("probe_detail=" + probe["detail"])
print("capability_status=ready")
print("capability_probe_status=" + cap["probe_status"])
print("capability_checked_at=" + cap["probe_checked_at"])
print("probe_container_cleanup=passed")
print("probe_ledger_cleanup=passed")
PY`, shellQuoteL4(remoteDir), shellQuoteL4(serverURL), shellQuoteL4(adminPassword), shellQuoteL4(masterSecret), port,
		shellQuoteL4(`{"username":"admin","password":"`+adminPassword+`"}`), serverURL)

	runCtx, runCancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer runCancel()
	out, err := partitionSSHCommand(runCtx, host, jump, remoteRun).CombinedOutput()
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", string(out))
	if err != nil {
		diagnosticsCtx, diagnosticsCancel := context.WithTimeout(context.Background(), time.Minute)
		defer diagnosticsCancel()
		diagnostics := fmt.Sprintf(`set +e
dir=%s
printf '%s\n' 'server-log:'
tail -n 120 "$dir/server.log" 2>&1
printf '%s\n' 'agent-log:'
tail -n 160 "$dir/agent.stdout" "$dir/agent-state/agent.log" 2>&1 | sed -E 's/(--token )[[:alnum:]_-]+/\1[redacted]/g'`, shellQuoteL4(remoteDir), "%s", "%s")
		diagnosticOut, diagnosticErr := partitionSSHCommand(diagnosticsCtx, host, jump, diagnostics).CombinedOutput()
		t.Fatalf("PPU automatic agent probe: %v; diagnostics_err=%v\n%s\n%s", err, diagnosticErr, out, diagnosticOut)
	}
	requireProbeContains(t, string(out),
		"host_hggc_runtime=N/A",
		"framework=hggc version=<empty>",
		"probe_image_after=loaded",
		"probe_status=passed",
		"total_bytes=4294967296",
		"quota_mib=4096",
		"capability_status=ready",
		"probe_container_cleanup=passed",
		"probe_ledger_cleanup=passed",
	)
}

func randomL4Secret(t *testing.T) string {
	t.Helper()
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(value[:])
}
