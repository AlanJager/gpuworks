//go:build cloudgpu && partition

package l4

import (
	"bytes"
	"compress/gzip"
	"context"
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

// ascendNerdctlVerifyLinuxARM64Gzip builds the tiny helper this case streams to
// the aarch64 fixture.
//
// It is built here, from the revision under test, rather than shipped as a
// checked-in artifact. The helper links a large slice of the agent (the Ascend
// prober, the runtime drivers, the embedded assets), so a committed binary had
// to be regenerated whenever any of those packages changed — and each MR paid a
// gate that rebuilt it to notice. Building it here is cheaper than that
// bookkeeping and strictly more honest: the binary is always the one this
// revision produces. The GPU node still receives only a binary; the L4 job
// already runs inside the Go image, so nothing new is required of it.
func ascendNerdctlVerifyLinuxARM64Gzip(t *testing.T) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repository path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "ascend-nerdctl-verify")
	cmd := exec.Command("go", "build", "-trimpath", "-o", binary, "./test/l4/cmd/ascend-nerdctl-verify")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ascend-nerdctl-verify for linux/arm64: %v: %s", err, out)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	// Stream it compressed: the helper is ~20 MiB and the fixtures sit behind a
	// slow link, but they always have gunzip.
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

// TestAscendNerdctlDeviceInjectionEndToEnd proves the nerdctl driver-aware
// Ascend injection (gpu.AscendProber.RuntimeInjectionForDriver) actually
// makes an NPU visible inside a real nerdctl container on a containerd-only
// host (the pool members 172.30.8.31/.32 run nerdctl against a shared
// containerd; .32 also hosts k8s pods in its k8s.io namespace while the
// agent keeps its own containers in the gpuworks namespace). It
// builds and streams the tiny helper (test/l4/cmd/ascend-nerdctl-verify) that calls the production
// injection and nerdctl driver functions engine.applyDeviceInjection uses,
// rather than replaying hand-written shell commands —
// nerdctl is a local CLI, not remote-addressable, so the verification code
// itself must run on the host, and the GPU node has no Go toolchain. See
// docs/dev/ledgers/engineering-log.md "Ascend
// device injection for the nerdctl runtime driver".
func TestAscendNerdctlDeviceInjectionEndToEnd(t *testing.T) {
	out := runAscendNerdctlVerifier(t, "device")
	requireProbeContains(t, out, "AVAILABLE True", "COUNT 1", "VERIFY_OK")
}

// TestAscendNerdctlVerifyHelperBuildsForTheFixture keeps the one useful half of
// the old artifact gate: the helper must still cross-compile for the fixture.
// It runs here, in the lane that needs the helper, instead of making every MR
// rebuild a checked-in binary to notice that it went stale.
func TestAscendNerdctlVerifyHelperBuildsForTheFixture(t *testing.T) {
	if helper := ascendNerdctlVerifyLinuxARM64Gzip(t); len(helper) < 1<<20 {
		t.Fatalf("helper gzip is implausibly small: %d bytes", len(helper))
	}
}

// TestAscendNerdctlPartitionCapabilityProbeEndToEnd runs the same tiny,
// behavioral probe that backs the node's PartitionProbes report. It
// uses the production workload builder + nerdctl path and verifies the 4 GiB
// dsmi slice view without a model, framework, or serving image.
func TestAscendNerdctlPartitionCapabilityProbeEndToEnd(t *testing.T) {
	out := runAscendNerdctlVerifier(t, "partition")
	requireProbeContains(t, out,
		"memory-quota=4096",
		"backend=ascend",
		"total_kb=4194304",
		"quota_mib=4096",
		"PARTITION_VERIFY_OK",
		"VERIFY_OK",
	)
}

func runAscendNerdctlVerifier(t *testing.T, mode string) string {
	t.Helper()
	image := hostEnv("GPUWORKS_ASCEND_NERDCTL_L4_IMAGE", "quay.io/ascend/sglang:v0.5.14-cann9.0.0-910b")
	preflightImage := ""
	if mode == "device" {
		preflightImage = image
	}
	host, jump, card := selectAscendNerdctlPoolMember(t, preflightImage)
	if _, err := strconv.Atoi(card); err != nil {
		t.Fatalf("GPUWORKS_ASCEND_NERDCTL_L4_CARD must be a physical numeric card index: %q", card)
	}

	runID := "gpuworks-ascend-nerdctl-verify-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteBin := "/tmp/" + runID

	// Build the helper from this revision and stream it over SSH stdin: only the
	// GPU node is assumed to lack a Go toolchain.
	helper := ascendNerdctlVerifyLinuxARM64Gzip(t)
	copyCtx, copyCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer copyCancel()
	copyCmd := partitionSSHCommand(copyCtx, host, jump, "cat > "+shellQuoteL4(remoteBin+".gz")+" && gunzip -f "+shellQuoteL4(remoteBin+".gz")+" && chmod +x "+shellQuoteL4(remoteBin))
	copyCmd.Stdin = bytes.NewReader(helper)
	if out, err := copyCmd.CombinedOutput(); err != nil {
		t.Fatalf("copy ascend-nerdctl-verify: %v: %s", err, out)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		// The helper owns exact workload cleanup. Do not glob-remove containers
		// here: partition failures intentionally preserve the final container and
		// state directory so CI logs can name a useful debug scene.
		cleanup := "rm -f " + shellQuoteL4(remoteBin)
		if out, err := partitionSSHCommand(cleanupCtx, host, jump, cleanup).CombinedOutput(); err != nil {
			t.Errorf("cleanup ascend-nerdctl-verify: %v: %s", err, out)
		}
	})

	runCtx, runCancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer runCancel()
	run := fmt.Sprintf("chmod +x %s && GPUWORKS_KEEP_ON_FAILURE=1 ASCEND_NERDCTL_VERIFY_MODE=%s ASCEND_NERDCTL_VERIFY_IMAGE=%s ASCEND_NERDCTL_VERIFY_INDEXES=%s %s",
		shellQuoteL4(remoteBin), shellQuoteL4(mode), shellQuoteL4(image), shellQuoteL4(card), shellQuoteL4(remoteBin))
	out, err := partitionSSHCommand(runCtx, host, jump, run).CombinedOutput()
	combined := string(out)
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", combined)
	if err != nil {
		t.Fatalf("Ascend nerdctl device injection: %v\n%s", err, out)
	}
	return combined
}

func selectAscendNerdctlPoolMember(t *testing.T, image string) (host, jump, card string) {
	t.Helper()
	type fixture struct{ host, jump, card string }
	// The card index is optional per member: an explicit CARD_1/CARD_2 (or
	// the shared CARD) wins; otherwise the preflight takes the first card
	// the member's own npu-smi lists. Pool members get repartitioned — one
	// dropped from eight NPUs to two mid-vNPU conversion while still passing
	// the nerdctl preflight — and a fixed default silently targets a card
	// that no longer exists on that host.
	fixtures := []fixture{
		{hostEnv("GPUWORKS_ASCEND_HOST", hostEnv("GPUWORKS_ASCEND_PRIMARY_HOST", "")), hostEnv("GPUWORKS_ASCEND_SSH_JUMP", ""), hostEnv("GPUWORKS_ASCEND_NERDCTL_L4_CARD_1", hostEnv("GPUWORKS_ASCEND_NERDCTL_L4_CARD", ""))},
		{hostEnv("GPUWORKS_ASCEND_HOST_2", ""), hostEnv("GPUWORKS_ASCEND_SSH_JUMP_2", ""), hostEnv("GPUWORKS_ASCEND_NERDCTL_L4_CARD_2", hostEnv("GPUWORKS_ASCEND_NERDCTL_L4_CARD", ""))},
	}
	var diagnostics []string
	for _, candidate := range fixtures {
		if candidate.host == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		preflight := `fail() { echo "ascend nerdctl preflight: $1" >&2; exit 1; }
test "$(uname -m)" = aarch64 || fail "arch is $(uname -m), want aarch64"
command -v nerdctl >/dev/null || fail "nerdctl missing on PATH"
nerdctl info >/dev/null 2>&1 || fail "nerdctl info failed (containerd not running?)"`
		if image != "" {
			preflight += "\nnerdctl -n gpuworks image inspect " + shellQuoteL4(image) + " >/dev/null 2>&1 || fail 'acceptance image not present in nerdctl gpuworks namespace'"
		}
		// npu-smi renders each NPU as "| <id>  <name> ..."; field 2 of the
		// first such row is the card the verifier may use.
		preflight += `
card="$(npu-smi info 2>/dev/null | awk '/^\| [0-9]+/ {print $2; exit}')"
[ -n "$card" ] || fail "npu-smi lists no card"
echo "ASCEND_CARD=$card"`
		out, err := partitionSSHCommand(ctx, candidate.host, candidate.jump, preflight).CombinedOutput()
		cancel()
		if err == nil {
			card := candidate.card
			if card == "" {
				card = parseAscendPreflightCard(string(out))
			}
			if card == "" {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: preflight named no card: %s", candidate.host, strings.TrimSpace(string(out))))
				continue
			}
			t.Logf("Ascend pool selected nerdctl-capable member %s card %s", candidate.host, card)
			return candidate.host, candidate.jump, card
		}
		diagnostics = append(diagnostics, fmt.Sprintf("%s: %v: %s", candidate.host, err, strings.TrimSpace(string(out))))
	}
	message := "no Ascend pool member currently exposes the cached nerdctl acceptance image"
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("%s: %s", message, strings.Join(diagnostics, "; "))
	}
	skipOrFailExternal(t, "%s: %s", message, strings.Join(diagnostics, "; "))
	return "", "", ""
}

// parseAscendPreflightCard reads the ASCEND_CARD=<id> line the preflight
// prints from the member's npu-smi listing; "" when the output names no
// usable card.
func parseAscendPreflightCard(out string) string {
	for _, line := range strings.Split(out, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "ASCEND_CARD=")
		if !ok || value == "" {
			continue
		}
		if _, err := strconv.Atoi(value); err == nil {
			return value
		}
	}
	return ""
}
