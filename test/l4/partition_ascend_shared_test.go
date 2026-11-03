//go:build cloudgpu && partition

package l4

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAscendSameCardDualContainerMemoryIsolation proves that device-share is
// not merely a reported flag. Two vcann-rt containers run concurrently on the
// same physical card, use distinct virtual IDs and the same VDie ledger ID,
// allocate real NPU memory, and independently enforce their 8 GiB ceilings.
func TestAscendSameCardDualContainerMemoryIsolation(t *testing.T) {
	host := requiredHostEnv(t, "GPUWORKS_ASCEND_PRIMARY_HOST")
	jump := hostEnv("GPUWORKS_ASCEND_SSH_JUMP", "")
	image := requiredHostEnv(t, "GPUWORKS_ASCEND_L4_IMAGE")
	card := hostEnv("GPUWORKS_ASCEND_L4_CARD", "3")
	if _, err := strconv.Atoi(card); err != nil {
		t.Fatalf("GPUWORKS_ASCEND_L4_CARD must be a physical numeric card index: %q", card)
	}
	runID := "gpuworks-ascend-shared-l4-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteDir := "/tmp/" + runID
	firstName, secondName := runID+"-a", runID+"-b"

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repository path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	asset := filepath.Join(repo, "internal", "assets", "bin", "runtime-assets", "ascend", "linux-arm64", "libvruntime.so")
	if stat, err := os.Stat(asset); err != nil || stat.Size() < 4096 {
		t.Fatalf("Ascend shared-container asset %s is unavailable", asset)
	}

	prepareCtx, prepareCancel := context.WithTimeout(t.Context(), time.Minute)
	defer prepareCancel()
	prepare := fmt.Sprintf(`set -eu
card=%s
dir=%s
docker image inspect %s >/dev/null
npu-smi info | grep -Fq %s
mkdir "$dir"
mkdir "$dir/sync"
board=$(npu-smi info -t board -i "$card" -c 0)
vdie=$(printf '%%s\n' "$board" | awk -F: '/VDie ID/{gsub(/^[ \t]+|[ \t]+$/, "", $2); gsub(/[ \t]+/, "-", $2); print $2; exit}')
test -n "$vdie"
share=$(npu-smi info -t device-share -i "$card" -c 0 || true)
if ! printf '%%s\n' "$share" | grep -qi true; then
  printf 'Y\n' | npu-smi set -t device-share -i "$card" -c 0 -d 1 >/dev/null
fi
npu-smi info -t device-share -i "$card" -c 0 | grep -qi true
printf '%%s\n' /usr/local/dcmi/libdcmi.so /usr/local/Ascend/driver/lib64/driver/libdcmi.so /opt/gpuworks/partition/libvruntime.so >"$dir/ld.so.preload"
for item in a:0 b:1; do
  name=${item%%:*}
  vid=${item##*:}
  cat >"$dir/npu-$name.config" <<EOF
physical-npu-id=$card
virtual-npu-id=$vid
aicore-quota=100
memory-quota=8192
shm-id=$vdie
scheduling-policy=2
EOF
done
cat >"$dir/allocate.py" <<'PY'
import os, time, torch
name = os.environ["SLICE_NAME"]
peer = "b" if name == "a" else "a"
free, total = torch.npu.mem_get_info(0)
print(f"{name}_slice_total_mib={total // 1024 // 1024}", flush=True)
allocation = torch.ones(2 * 1024 * 1024 * 1024, dtype=torch.uint8, device="npu")
print(f"{name}_allocation_mib={allocation.numel() // 1024 // 1024}", flush=True)
open(f"/sync/{name}.ready", "w").write("ready\n")
deadline = time.time() + 60
while not os.path.exists(f"/sync/{peer}.ready"):
    if time.time() >= deadline:
        raise SystemExit(f"peer {peer} did not allocate concurrently")
    time.sleep(0.1)
print(f"{name}_peer_seen=yes", flush=True)
try:
    torch.ones(9 * 1024 * 1024 * 1024, dtype=torch.uint8, device="npu")
except (RuntimeError, torch.OutOfMemoryError) as error:
    print(f"{name}_over_quota_denied={str(error).splitlines()[0]}", flush=True)
else:
    raise SystemExit("unexpected: 9 GiB allocation succeeded under 8 GiB quota")
time.sleep(2)
print(f"{name}_complete=yes", flush=True)
PY
printf 'vdie=%%s\n' "$vdie"`, shellQuoteL4(card), shellQuoteL4(remoteDir), shellQuoteL4(image), shellQuoteL4("No running processes found in NPU "+card))
	prepareOut, err := partitionSSHCommand(prepareCtx, host, jump, prepare).CombinedOutput()
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("prepare idle Ascend shared card %s: %v\n%s", card, err, prepareOut)
		}
		skipOrFailExternal(t, "Ascend shared-container fixture unavailable or card %s busy: %v", card, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		cleanup := fmt.Sprintf(`set +e
docker rm -f %s %s >/dev/null 2>&1
dir=%s
case "$dir" in /tmp/gpuworks-ascend-shared-l4-*) rm -rf -- "$dir" ;; *) exit 2 ;; esac`, shellQuoteL4(firstName), shellQuoteL4(secondName), shellQuoteL4(remoteDir))
		if out, cleanupErr := partitionSSHCommand(cleanupCtx, host, jump, cleanup).CombinedOutput(); cleanupErr != nil {
			t.Errorf("cleanup Ascend shared-container L4: %v: %s", cleanupErr, out)
		}
	})

	copyCtx, copyCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer copyCancel()
	if out, err := partitionSCPCommand(copyCtx, jump, []string{asset}, host+":"+remoteDir+"/libvruntime.so").CombinedOutput(); err != nil {
		t.Fatalf("copy Ascend shared-container asset: %v: %s", err, out)
	}

	run := fmt.Sprintf(`set -eu
dir=%s
card=%s
image=%s
image_ld=$(docker image inspect "$image" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^LD_LIBRARY_PATH=//p' | head -n 1)
test -n "$image_ld"
run_slice() {
  slice="$1"
  container="$2"
  docker run -d --name "$container" \
    --runtime=ascend --shm-size=16g \
    -e ASCEND_VISIBLE_DEVICES="$card" \
    -e ASCEND_RT_VISIBLE_DEVICES=0 -e NPU_VISIBLE_DEVICES=0 \
    -e ENPU_LOG_LEVEL=1 -e ENPU_DSMI_HOOK=1 -e SLICE_NAME="$slice" \
    -e LD_LIBRARY_PATH="/usr/local/Ascend/driver/lib64/common:/usr/local/Ascend/driver/lib64/driver:/usr/local/Ascend/driver/lib64:$image_ld" \
    -v "$dir/ld.so.preload:/etc/ld.so.preload:ro" \
    -v "$dir/npu-$slice.config:/etc/enpu/vcann-rt/npu_info.config:ro" \
    -v "$dir/libvruntime.so:/opt/gpuworks/partition/libvruntime.so:ro" \
    -v "$dir/allocate.py:/allocate.py:ro" -v "$dir/sync:/sync" -v /dev/shm:/dev/shm \
    --entrypoint python3 "$image" -u /allocate.py
}
run_slice a %s
run_slice b %s
code_a=$(docker wait %s)
code_b=$(docker wait %s)
logs_a=$(docker logs %s 2>&1)
logs_b=$(docker logs %s 2>&1)
printf '%%s\n' "$logs_a" "$logs_b"
test "$code_a" = 0
test "$code_b" = 0
printf '%%s\n' "$logs_a" | grep -q '^a_slice_total_mib=8192$'
printf '%%s\n' "$logs_b" | grep -q '^b_slice_total_mib=8192$'
printf '%%s\n' "$logs_a" | grep -q '^a_allocation_mib=2048$'
printf '%%s\n' "$logs_b" | grep -q '^b_allocation_mib=2048$'
printf '%%s\n' "$logs_a" | grep -q '^a_peer_seen=yes$'
printf '%%s\n' "$logs_b" | grep -q '^b_peer_seen=yes$'
printf '%%s\n' "$logs_a" | grep -q '^a_over_quota_denied='
printf '%%s\n' "$logs_b" | grep -q '^b_over_quota_denied='
printf '%%s\n' "$logs_a" | grep -q '^a_complete=yes$'
printf '%%s\n' "$logs_b" | grep -q '^b_complete=yes$'
vid_a=$(awk -F= '/virtual-npu-id/{print $2}' "$dir/npu-a.config")
vid_b=$(awk -F= '/virtual-npu-id/{print $2}' "$dir/npu-b.config")
shm_a=$(awk -F= '/shm-id/{print $2}' "$dir/npu-a.config")
shm_b=$(awk -F= '/shm-id/{print $2}' "$dir/npu-b.config")
test "$vid_a" = 0
test "$vid_b" = 1
test "$shm_a" = "$shm_b"
printf 'virtual_ids=%%s,%%s\nshared_shm_id=%%s\ndevice_share=true\n' "$vid_a" "$vid_b" "$shm_a"
docker rm %s %s >/dev/null
test -z "$(docker ps -a --format '{{.Names}}' | grep '^%s-[ab]$' || true)"
	echo dual_container_cleanup=passed`, shellQuoteL4(remoteDir), shellQuoteL4(card), shellQuoteL4(image),
		shellQuoteL4(firstName), shellQuoteL4(secondName), shellQuoteL4(firstName), shellQuoteL4(secondName), shellQuoteL4(firstName), shellQuoteL4(secondName),
		shellQuoteL4(firstName), shellQuoteL4(secondName), runID)
	runCtx, runCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer runCancel()
	out, err := partitionSSHCommand(runCtx, host, jump, run).CombinedOutput()
	combined := string(prepareOut) + string(out)
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", combined)
	if err != nil {
		t.Fatalf("Ascend same-card dual-container memory isolation: %v\n%s", err, combined)
	}
	requireProbeContains(t, combined,
		"a_slice_total_mib=8192",
		"b_slice_total_mib=8192",
		"a_allocation_mib=2048",
		"b_allocation_mib=2048",
		"a_peer_seen=yes",
		"b_peer_seen=yes",
		"a_over_quota_denied=",
		"b_over_quota_denied=",
		"virtual_ids=0,1",
		"device_share=true",
		"dual_container_cleanup=passed",
	)
}
