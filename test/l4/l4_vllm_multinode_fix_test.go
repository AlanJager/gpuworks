//go:build cloudgpu && compshare

package l4

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	distributedVLLMFixHostDir      = "/tmp/gpuworks-vllm-inner-dp-86e1060b"
	distributedVLLMFixContainerDir = "/gpuworks-vllm-inner-dp-fix"
	distributedVLLMFixFile         = distributedVLLMFixHostDir + "/sitecustomize.py"
)

// distributedVLLMFixScript carries the exact behavioral change from upstream
// vLLM commit 86e1060b17d9042ab8f7b7baba26b1d6cbc36c2b (#35892). The
// CUDA 12.8 runner currently available to CompShare uses vLLM 0.17.1, whose
// WorkerProc initializes multi-node message queues before init_device creates
// inner_dp_world. Python loads this file through PYTHONPATH before importing
// vLLM. It patches only the exact affected source shape and fails closed for
// unknown versions.
const distributedVLLMFixScript = `import glob
import os

marker = "# Initialize message queues after init_device() since multi-node setups"
early = """        # Load model
        self._init_message_queues(input_shm_handle, vllm_config)
"""
anchor = """
        # Enable environment variable cache (e.g. assume no more
"""
late = """
        # Initialize message queues after init_device() since multi-node setups
        # (nnodes_within_dp > 1) require distributed groups to be initialized
        self._init_message_queues(input_shm_handle, vllm_config)
"""

override = os.environ.get("GPUWORKS_VLLM_INNER_DP_FILE", "").strip()
paths = [override] if override else sorted(
    glob.glob("/usr/local/lib/python*/dist-packages/vllm/v1/executor/multiproc_executor.py")
    + glob.glob("/usr/local/lib/python*/site-packages/vllm/v1/executor/multiproc_executor.py")
)
errors = []
for path in paths:
    if not path or not os.path.isfile(path):
        continue
    with open(path, "r", encoding="utf-8") as stream:
        source = stream.read()
    if marker in source and early not in source:
        print("GPUWORKS_VLLM_INNER_DP_FIX already-applied " + path, flush=True)
        break
    if source.count(early) != 1 or source.count(anchor) != 1:
        errors.append(path + ": unexpected source shape")
        continue
    patched = source.replace(early, "        # Load model\n", 1)
    patched = patched.replace(anchor, late + anchor, 1)
    temporary = path + ".gpuworks-inner-dp.tmp"
    with open(temporary, "w", encoding="utf-8") as stream:
        stream.write(patched)
    os.replace(temporary, path)
    print("GPUWORKS_VLLM_INNER_DP_FIX applied " + path, flush=True)
    break
else:
    detail = "; ".join(errors) if errors else "vLLM multiproc_executor.py not found"
    raise RuntimeError("GPUWorks vLLM inner_dp workaround refused: " + detail)
`

func installDistributedVLLMFix(t *testing.T, envs []*cloudTestEnv) {
	t.Helper()
	digest := sha256.Sum256([]byte(distributedVLLMFixScript))
	encoded := base64.StdEncoding.EncodeToString([]byte(distributedVLLMFixScript))
	command := fmt.Sprintf(`set -eu
root=""
if [ "$(id -u)" -ne 0 ]; then root="sudo -n"; fi
$root install -d -m 0755 %s
printf %%s %s | base64 -d | $root tee %s >/dev/null
$root chmod 0444 %s
`, shellQuoteForTest(distributedVLLMFixHostDir), shellQuoteForTest(encoded), shellQuoteForTest(distributedVLLMFixFile), shellQuoteForTest(distributedVLLMFixFile))
	for rank, env := range envs {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		out, err := runCloudCommand(ctx, env.Instance, command)
		cancel()
		if err != nil {
			t.Fatalf("install vLLM inner_dp fix on rank %d: %v (%s)", rank, err, strings.TrimSpace(out))
		}
	}
	t.Logf("installed upstream vLLM #35892 workaround on %d VM(s): sha256=%x", len(envs), digest)
}

func distributedVLLMContainerOverrides() map[string]any {
	return map[string]any{
		"env": map[string]string{"PYTHONPATH": distributedVLLMFixContainerDir},
		"extra_mounts": []map[string]any{{
			"host_path": distributedVLLMFixHostDir, "container_path": distributedVLLMFixContainerDir,
			"read_only": true, "purpose": "vllm-upstream-86e1060b",
		}},
	}
}

func TestDistributedVLLMContainerOverridesMountExactFix(t *testing.T) {
	overrides := distributedVLLMContainerOverrides()
	env, ok := overrides["env"].(map[string]string)
	if !ok || env["PYTHONPATH"] != distributedVLLMFixContainerDir {
		t.Fatalf("distributed fix env=%#v", overrides["env"])
	}
	mounts, ok := overrides["extra_mounts"].([]map[string]any)
	if !ok || len(mounts) != 1 || mounts[0]["host_path"] != distributedVLLMFixHostDir || mounts[0]["container_path"] != distributedVLLMFixContainerDir || mounts[0]["read_only"] != true {
		t.Fatalf("distributed fix mounts=%#v", overrides["extra_mounts"])
	}
}

func TestDistributedVLLMFixAppliesExactUpstreamOrderingAndIsIdempotent(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to execute the vLLM patch fixture")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "multiproc_executor.py")
	script := filepath.Join(dir, "sitecustomize.py")
	fixture := `class WorkerProc:
    def __init__(self, input_shm_handle, vllm_config):
        # Load model
        self._init_message_queues(input_shm_handle, vllm_config)
        is_eep_new_worker = False
        if not is_eep_new_worker:
            self.worker.init_device()
            self.worker.load_model()

        # Enable environment variable cache (e.g. assume no more
        enable_envs_cache()
`
	if err := os.WriteFile(target, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(distributedVLLMFixScript), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		cmd := exec.Command(python, script)
		cmd.Env = append(os.Environ(), "PYTHONPATH="+dir, "GPUWORKS_VLLM_INNER_DP_FILE="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("patch attempt %d: %v (%s)", attempt, err, strings.TrimSpace(string(out)))
		}
	}
	patched, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patched)
	initDevice := strings.Index(text, "self.worker.init_device()")
	initQueues := strings.Index(text, "self._init_message_queues(input_shm_handle, vllm_config)")
	if initDevice < 0 || initQueues < 0 || initQueues < initDevice || strings.Count(text, markerLine()) != 1 {
		t.Fatalf("patched ordering is not the upstream fix:\n%s", text)
	}
}

func markerLine() string {
	return "# Initialize message queues after init_device() since multi-node setups"
}
