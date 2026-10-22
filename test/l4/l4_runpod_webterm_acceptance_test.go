//go:build cloudgpu && runpod

package l4

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

const cudaHolderSource = `#include <dlfcn.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
typedef int CUresult;
typedef int CUdevice;
typedef struct CUctx_st *CUcontext;
typedef uint64_t CUdeviceptr;
int main(void) {
  void *lib = dlopen("libcuda.so.1", RTLD_NOW);
  if (!lib) { fprintf(stderr, "GPU_HOLDER_ERROR=dlopen:%s\n", dlerror()); return 2; }
  CUresult (*cuInit)(unsigned) = dlsym(lib, "cuInit");
  CUresult (*cuDeviceGet)(CUdevice *, int) = dlsym(lib, "cuDeviceGet");
  CUresult (*cuCtxCreate)(CUcontext *, unsigned, CUdevice) = dlsym(lib, "cuCtxCreate_v2");
  CUresult (*cuMemAlloc)(CUdeviceptr *, size_t) = dlsym(lib, "cuMemAlloc_v2");
  if (!cuInit || !cuDeviceGet || !cuCtxCreate || !cuMemAlloc) { fprintf(stderr, "GPU_HOLDER_ERROR=dlsym\n"); return 3; }
  CUdevice dev;
  CUcontext ctx;
  CUdeviceptr ptr;
  CUresult rc;
  if ((rc = cuInit(0))) { fprintf(stderr, "GPU_HOLDER_ERROR=cuInit:%d\n", rc); return 4; }
  if ((rc = cuDeviceGet(&dev, 0))) { fprintf(stderr, "GPU_HOLDER_ERROR=cuDeviceGet:%d\n", rc); return 4; }
  if ((rc = cuCtxCreate(&ctx, 0, dev))) { fprintf(stderr, "GPU_HOLDER_ERROR=cuCtxCreate:%d\n", rc); return 4; }
  if ((rc = cuMemAlloc(&ptr, 64u << 20))) { fprintf(stderr, "GPU_HOLDER_ERROR=cuMemAlloc:%d\n", rc); return 4; }
  signal(SIGHUP, SIG_IGN);
  signal(SIGTERM, SIG_IGN);
  printf("GPU_HOLDER_READY=%d\n", getpid());
  fflush(stdout);
  sleep(300);
  return 0;
}`

// TestRunpodUdockerWebtermRuntimeProbe rents an isolated RunPod container,
// installs the current agent through the real provider path, and records the
// udocker command surface needed before enabling container Web Terminal.
// The test distinguishes udocker's fresh proot shell from Docker attach while
// exercising the production server/agent WebSocket PTY path end to end.
func TestRunpodUdockerWebtermRuntimeProbe(t *testing.T) {
	p := runpodProvider(t)
	env := testenv.New(t)
	cloud := setupCloudNode(t, env, p, cloudprovider.GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 8192, DiskGB: 40})
	requireOnlineNode(t, cloud)
	// Use the published scratch engine and a locally registered model directory
	// to force the production udocker Prepare/Run path without downloading model
	// weights. This gives the probe a real long-lived container process to compare
	// against any future terminal shell.
	d := deploySeedInferenceTemplateFake(t, cloud, runpodInferenceTemplateFakeImage(), "webterm-runtime-probe")
	if len(d.instances) != 1 || strings.TrimSpace(d.instances[0].ContainerID) == "" {
		t.Fatalf("fake deployment did not expose a container id: %+v", d.instances)
	}
	out, err := runCloudCommand(t.Context(), cloud.Instance, `set +e
echo "== runtime =="
find /opt/gpuworks -maxdepth 5 -type f \( -name 'udocker.pyz' -o -name 'udocker.py' -o -name 'proot-*' \) -printf '%p\n' 2>/dev/null | sort | head -40
echo "== udocker invocation =="
UDOCKER_DIR=/opt/gpuworks/udocker UDOCKER_USE_PROOT_EXECUTABLE=/opt/gpuworks/udocker/bin/proot-x86_64 python3 /opt/gpuworks/offline/udocker/udocker/udocker --allow-root --version 2>&1 || true
echo "== exec help =="
UDOCKER_DIR=/opt/gpuworks/udocker UDOCKER_USE_PROOT_EXECUTABLE=/opt/gpuworks/udocker/bin/proot-x86_64 python3 /opt/gpuworks/offline/udocker/udocker/udocker --allow-root exec --help 2>&1 || true
echo "== run help =="
UDOCKER_DIR=/opt/gpuworks/udocker UDOCKER_USE_PROOT_EXECUTABLE=/opt/gpuworks/udocker/bin/proot-x86_64 python3 /opt/gpuworks/offline/udocker/udocker/udocker --allow-root run --help 2>&1 | head -80 || true
echo "== processes =="
ps -eo pid=,ppid=,pgid=,comm=,args= | grep -E 'gpuworks-agent|udocker|proot|fake-engine' | grep -v grep || true`)
	if err != nil {
		t.Fatalf("runtime probe failed: %v: %s", err, strings.TrimSpace(out))
	}
	for _, want := range []string{
		"version: 1.3.17",
		"Error: invalid command: exec",
		"/opt/gpuworks/udocker/bin/proot-x86_64",
		"-b /proc:/proc",
		"-r /opt/gpuworks/udocker/containers/",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("runtime probe output missing %q:\n%s", want, strings.TrimSpace(out))
		}
	}
	t.Logf("runpod udocker runtime probe:\n%s", strings.TrimSpace(out))
	t.Logf("model container id=%s", d.instances[0].ContainerID)
	// The published fake image is scratch-like and intentionally has no shell.
	// Install the host's static busybox into this workload rootfs solely as a
	// shell probe fixture; this does not touch the model process or its image.
	holderSource := base64.StdEncoding.EncodeToString([]byte(cudaHolderSource))
	rootfsOut, rootfsErr := runCloudCommand(t.Context(), cloud.Instance, fmt.Sprintf(`set -eu
	root="$(ps -eo args= | sed -n 's#.* -r \([^ ]*/ROOT\) -w .*#\1#p' | head -1)"
	[ -d "$root" ]
	printf %%s %s | base64 -d > /tmp/gw-gpu-holder.c
	gcc -O2 -o /tmp/gw-gpu-holder /tmp/gw-gpu-holder.c -ldl
	mkdir -p "$root/bin"
	cp "$(readlink -f /bin/sh)" "$root/bin/sh"
	mkdir -p "$root/usr/bin"
	cp /usr/bin/stty "$root/usr/bin/"
	cp /tmp/gw-gpu-holder "$root/usr/bin/"
	mkdir -p "$root/lib/x86_64-linux-gnu" "$root/lib64"
	cp /lib/x86_64-linux-gnu/libc.so.6 "$root/lib/x86_64-linux-gnu/"
	for lib in libm.so.6 libdl.so.2 libpthread.so.0 librt.so.1; do
		[ ! -f "/lib/x86_64-linux-gnu/$lib" ] || cp "/lib/x86_64-linux-gnu/$lib" "$root/lib/x86_64-linux-gnu/"
	done
	ldd "$root/opt/nvidia-shim/libcuda.so.1" | awk '{for (i=1; i<=NF; i++) if ($i ~ /^\//) print $i}' | while read -r lib; do
		mkdir -p "$root$(dirname "$lib")"
		cp "$lib" "$root$lib"
	done
	cp /lib64/ld-linux-x86-64.so.2 "$root/lib64/"
	chmod 0755 "$root/bin/sh"
	printf 'SHELL_ROOTFS=%%s\n' "$root"`, shellQuoteForTest(holderSource)))
	if rootfsErr != nil {
		t.Fatalf("prepare shell-capable rootfs: %v: %s", rootfsErr, strings.TrimSpace(rootfsOut))
	}

	// A direct fresh `udocker run` proves that this is not an attach to the
	// model PID. The command intentionally carries the workload env/workdir,
	// matching the production ExecPTY reconstruction.
	cid := shellQuoteForTest(d.instances[0].ContainerID)
	inner := `printf 'SHELL_PID=%s SHELL_CWD=%s SHELL_VLLM_HOST_IP=%s\n' "$$" "$PWD" "${VLLM_HOST_IP-}"`
	shellProbe := fmt.Sprintf(`set +e
MODEL_PID="$(pgrep -n -x fake-engine)"
printf 'MODEL_PID=%%s MODEL_CMD=' "$MODEL_PID"
tr '\000' ' ' < /proc/$MODEL_PID/cmdline || true
printf '\n'
UDOCKER_DIR=/opt/gpuworks/udocker UDOCKER_USE_PROOT_EXECUTABLE=/opt/gpuworks/udocker/bin/proot-x86_64 python3 /opt/gpuworks/offline/udocker/udocker/udocker --allow-root run --user=1001 --workdir=/ --env=VLLM_HOST_IP=127.0.0.1 --entrypoint=/bin/sh %s -c %s
status=$?
printf 'FRESH_SHELL_STATUS=%%s\n' "$status"
exit 0`, cid, shellQuoteForTest(inner))
	shellOut, shellErr := runCloudCommand(t.Context(), cloud.Instance, shellProbe)
	if shellErr != nil {
		t.Fatalf("fresh udocker shell probe failed: %v: %s", shellErr, strings.TrimSpace(shellOut))
	}
	if !strings.Contains(shellOut, "MODEL_PID=") || !strings.Contains(shellOut, "SHELL_CWD=/") || !strings.Contains(shellOut, "SHELL_VLLM_HOST_IP=127.0.0.1") || !strings.Contains(shellOut, "FRESH_SHELL_STATUS=0") {
		t.Fatalf("fresh shell comparison did not capture the controlled-shell result:\n%s", strings.TrimSpace(shellOut))
	}
	t.Logf("fresh shell comparison:\n%s", strings.TrimSpace(shellOut))
	// Exercise the real server admission path against this udocker node. The
	// runtime-specific capability must be present before the server offers the
	// controlled workload shell.
	api := env.API(t)
	settingsResp, err := api.Get("/api/v1/nodes/" + cloud.NodeUUID + "/terminal/settings")
	if err != nil {
		t.Fatal(err)
	}
	settingsBody, _ := io.ReadAll(settingsResp.Body)
	settingsResp.Body.Close()
	if settingsResp.StatusCode != http.StatusOK {
		t.Fatalf("terminal settings status=%d body=%s", settingsResp.StatusCode, settingsBody)
	}
	var settings struct {
		ContainerSupported bool   `json:"container_supported"`
		ContainerRuntime   string `json:"container_runtime"`
		ContainerReason    string `json:"container_reason"`
	}
	if err := json.Unmarshal(settingsBody, &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.ContainerSupported || settings.ContainerRuntime != "udocker" || settings.ContainerReason != "" {
		t.Fatalf("udocker terminal preflight=%+v", settings)
	}
	t.Logf("real-node udocker controlled-shell preflight: supported=%t runtime=%s", settings.ContainerSupported, settings.ContainerRuntime)

	// Exercise the actual browser-facing WebSocket bridge: resize, command
	// echo, Ctrl-C delivery, and clean disconnect. The server opens the agent
	// PTY through ExecPTY, so this covers the production argv and process-group
	// teardown rather than only the CLI surface above.
	wsURL := "ws" + strings.TrimPrefix(env.URL(), "http") + "/api/v1/nodes/" + cloud.NodeUUID + "/terminal?target=container&ref=" + d.instances[0].ContainerID + "&rows=24&cols=80"
	wsCtx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + api.Token}}})
	if err != nil {
		t.Fatalf("udocker container terminal websocket: %v", err)
	}
	defer ws.CloseNow()
	if typ, data, err := ws.Read(wsCtx); err != nil || typ != websocket.MessageText || !strings.Contains(string(data), `"type":"info"`) {
		t.Fatalf("container terminal info frame type=%v err=%v data=%q", typ, err, data)
	}
	if err := ws.Write(wsCtx, websocket.MessageText, []byte(`{"type":"resize","rows":45,"cols":150}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(wsCtx, websocket.MessageBinary, []byte("printf 'WEBTERM_PID=%s CWD=%s ENV=%s\\n' \"$$\" \"$PWD\" \"${VLLM_HOST_IP-}\"; stty size; /usr/bin/gw-gpu-holder\n")); err != nil {
		t.Fatal(err)
	}
	var terminalData strings.Builder
	for !strings.Contains(terminalData.String(), "GPU_HOLDER_READY=") {
		typ, data, readErr := ws.Read(wsCtx)
		if readErr != nil {
			t.Fatalf("read container terminal command output: %v; output=%q", readErr, terminalData.String())
		}
		if typ == websocket.MessageBinary {
			terminalData.Write(data)
		}
		if strings.Contains(terminalData.String(), "GPU_HOLDER_ERROR=") {
			t.Fatalf("CUDA holder failed inside container shell: %q", terminalData.String())
		}
	}
	if err := ws.Write(wsCtx, websocket.MessageBinary, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(wsCtx, websocket.MessageBinary, []byte("echo AFTER_INT; exit\n")); err != nil {
		t.Fatal(err)
	}
	for terminalData.Len() < 4096 {
		typ, data, readErr := ws.Read(wsCtx)
		if readErr != nil {
			break
		}
		if typ == websocket.MessageBinary {
			terminalData.Write(data)
		}
		if strings.Contains(terminalData.String(), "AFTER_INT") {
			break
		}
	}
	gotTerm := terminalData.String()
	if !strings.Contains(gotTerm, "WEBTERM_PID=") || !strings.Contains(gotTerm, "45 150") || !strings.Contains(gotTerm, "ENV=127.0.0.1") || !strings.Contains(gotTerm, "AFTER_INT") {
		t.Fatalf("container terminal PTY exchange missing expected output: %q", gotTerm)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "interactive checks complete")

	// A second session is left running and then disconnected. Its proot/session
	// process group must disappear while the model PID remains alive.
	ws2, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + api.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = ws2.Read(wsCtx)
	_ = ws2.Write(wsCtx, websocket.MessageBinary, []byte("/usr/bin/gw-gpu-holder\n"))
	var holderData strings.Builder
	for !strings.Contains(holderData.String(), "GPU_HOLDER_READY=") {
		typ, data, readErr := ws2.Read(wsCtx)
		if readErr != nil {
			t.Fatalf("wait for GPU holder: %v", readErr)
		}
		if typ == websocket.MessageBinary {
			holderData.Write(data)
		}
	}
	holderPID := markerPID(holderData.String(), "GPU_HOLDER_READY=")
	if holderPID == "" {
		t.Fatalf("could not parse GPU holder PID from %q", holderData.String())
	}
	gpuOut, gpuErr := runCloudCommand(t.Context(), cloud.Instance, "nvidia-smi --query-compute-apps=pid,used_gpu_memory --format=csv,noheader,nounits | awk -F, '$1+0 == "+holderPID+" && $2+0 >= 64 {found=1} END {exit !found}'")
	if gpuErr != nil {
		t.Fatalf("terminal child did not hold expected GPU memory: %v: %s", gpuErr, strings.TrimSpace(gpuOut))
	}
	_ = ws2.Close(websocket.StatusNormalClosure, "disconnect cleanup")
	cleanupOut, cleanupErr := runCloudCommand(t.Context(), cloud.Instance, fmt.Sprintf(`set +e
	cid=%s
	terminal_alive() {
		ps -eo comm=,args= | awk -v cid="$cid" '($1 == "runuser" || $1 == "python3") && index($0, "--entrypoint=/bin/sh") && index($0, cid) { found=1 } END { exit !found }'
	}
	for _ in $(seq 1 15); do terminal_alive || break; sleep 1; done
	terminal_alive && exit 1
	pgrep -x gw-gpu-holder >/dev/null && exit 1 || true
	pgrep -x fake-engine >/dev/null
	echo MODEL_SURVIVED`, shellQuoteForTest(d.instances[0].ContainerID)))
	if cleanupErr != nil || !strings.Contains(cleanupOut, "MODEL_SURVIVED") {
		t.Fatalf("terminal disconnect cleanup/model isolation failed: %v: %s", cleanupErr, strings.TrimSpace(cleanupOut))
	}

	// Reuse the existing crash/reconnect path to verify that an agent restart
	// does not lose the running udocker workload or replace its container. The
	// seed deployment helper returns the generic cloudDeployment shape, so adapt
	// its single instance to the RunPod-specific crash helper.
	if len(d.instances) != 1 {
		t.Fatalf("expected one seed deployment instance before crash probe, got %d", len(d.instances))
	}
	runpodSeed := &runpodDeployment{
		env:          cloud,
		deploymentID: d.deploymentID,
		servedName:   d.servedName,
		instanceUUID: d.instances[0].UUID,
		containerID:  d.instances[0].ContainerID,
		apiKey:       d.apiKey,
	}
	testRunpodCrash(t, runpodSeed)

	// Delete through the product API and verify that workload/session processes
	// and per-container temporary trees are gone. Shared offline udocker assets
	// (including the fixed proot executable) are intentionally not treated as
	// leaks because they are part of the node installation.
	deleteID := d.deploymentID
	resp, deleteErr := cloud.Control.API(t).Delete(fmt.Sprintf("/api/v1/deployments/%d", deleteID))
	if deleteErr != nil {
		t.Fatalf("delete RunPod deployment: %v", deleteErr)
	}
	deleteBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete RunPod deployment status=%d body=%s", resp.StatusCode, deleteBody)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := cloud.Control.Store().GetDeployment(deleteID); err != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if _, err := cloud.Control.Store().GetDeployment(deleteID); err == nil {
		t.Fatalf("RunPod deployment %d was not deleted", deleteID)
	}
	const deleteProbe = `set +e
	if pgrep -x fake-engine >/dev/null; then echo FAKE_ENGINE_LEFT=1; exit 1; fi
	if pgrep -x proot >/dev/null; then echo PROOT_LEFT=1; exit 1; fi
	if find /opt/gpuworks /tmp/gpuworks -type d -name '.gpuworks-*' -print 2>/dev/null | grep -q .; then
		echo TEMP_ROOTFS_LEFT=1
		exit 1
	fi
	echo UDOCKER_DELETE_CLEAN
	exit 0`
	var postDelete string
	var postDeleteErr error
	cleanDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(cleanDeadline) {
		postDelete, postDeleteErr = runCloudCommand(t.Context(), cloud.Instance, deleteProbe)
		if postDeleteErr == nil && strings.Contains(postDelete, "UDOCKER_DELETE_CLEAN") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if postDeleteErr != nil || !strings.Contains(postDelete, "UDOCKER_DELETE_CLEAN") {
		t.Fatalf("udocker deletion left workload/session artifacts: %v: %s", postDeleteErr, strings.TrimSpace(postDelete))
	}
}

func markerPID(output, marker string) string {
	for _, field := range strings.Fields(output) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(field), marker); ok {
			value = strings.Trim(value, "\r\n")
			if value != "" {
				return value
			}
		}
	}
	return ""
}
