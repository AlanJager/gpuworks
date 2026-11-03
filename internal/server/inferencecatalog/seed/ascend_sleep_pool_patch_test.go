package seed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Qwen3.8-Flash-Next 910C (A3) spec ships a pre-start patch that scopes the
// level-1 sleep kv_cache memory pool to the KV backing allocation. The image's
// vllm-ascend wraps the whole initialize_kv_cache() in that discardable pool,
// so a level-1 sleep also frees the persistent metadata built there while
// keeping its virtual address; sleep and wake still report success and the
// first real forward pass afterwards faults the device. Upstream fixed this in
// vllm-ascend PR #16096, after this image was built.
//
// The fault itself only reproduces on a real 910C, so the hermetic assertion
// is structural: harness.py reports which statements sit inside the pool, and
// the tables below pin that scope before and after patching. The patch was
// also verified on hardware before it shipped (one prewarm plus three explicit
// sleep/wake cycles, each followed by real inference, with no crash); that run
// is what these tables keep from regressing, and it cannot run offline.
const (
	sleepPoolModelDir      = "catalog/models/qwen/qwen3.8-flash-next"
	sleepPoolAsset         = "assets/ascend-sleep-pool-patch.tar.gz"
	sleepPoolMember        = "ascend_sleep_pool_patch.py"
	sleepPoolUnpackPath    = "/opt/gpuworks/ascend-sleep-pool-patch"
	sleepPoolFixtureRoot   = "testdata/ascend-sleep-pool/vllm_ascend"
	sleepPoolHarness       = "testdata/ascend-sleep-pool/harness.py"
	sleepPoolBuilderSource = "../../../../build/inference-templates/flash-next-sleep-pool/" + sleepPoolMember
	sleepPoolSpecName      = "int8-vllm-ascend-a3-tp8"
	sleepPoolModelID       = "qwen/qwen3.8-flash-next"
)

// defectScope is what the pristine image sources report: the worker wraps the
// entire initialize_kv_cache() in the pool, and the runner neither takes nor
// scopes a pool of its own. Everything built during KV cache initialization is
// therefore inside the discardable pool.
var defectScope = map[string]string{
	"worker.forwards-pool":             "no",
	"worker.wraps-initialize-kv-cache": "yes",
	"runner.accepts-pool":              "no",
	"runner.allocation-in-pool":        "no",
	"runner.init-forwards-pool":        "no",
	"runner.persistent-in-pool":        "none",
}

// fixedScope is what the patched sources must report: the worker hands the pool
// down instead of wrapping the call, and the runner enters it only around the
// backing allocation, leaving reshape, cross-layer sharing and bind outside.
var fixedScope = map[string]string{
	"worker.forwards-pool":             "yes",
	"worker.wraps-initialize-kv-cache": "no",
	"runner.accepts-pool":              "yes",
	"runner.allocation-in-pool":        "yes",
	"runner.init-forwards-pool":        "yes",
	"runner.persistent-in-pool":        "none",
}

func sleepPoolPython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip("python3 not available: Ascend sleep-pool patch harness cannot run")
	}
	return python
}

// reportScope runs the harness and returns the observed pool-scope facts.
func reportScope(t *testing.T, python, root string) map[string]string {
	t.Helper()
	output, err := exec.Command(python, "-B", sleepPoolHarness, "scope", root).CombinedOutput()
	if err != nil {
		t.Fatalf("harness scope %s: %v\n%s", root, err, output)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			facts[fields[0]] = fields[1]
		}
	}
	if len(facts) == 0 {
		t.Fatalf("harness scope %s produced no facts:\n%s", root, output)
	}
	return facts
}

func assertScope(t *testing.T, got, want map[string]string, context string) {
	t.Helper()
	for key, expected := range want {
		if actual, ok := got[key]; !ok {
			t.Errorf("%s: harness did not report %s", context, key)
		} else if actual != expected {
			t.Errorf("%s: %s is %q, want %q", context, key, actual, expected)
		}
	}
	for key := range got {
		if _, known := want[key]; !known {
			t.Errorf("%s: harness reports unexpected fact %s; pin it in both tables", context, key)
		}
	}
}

// extractSleepPoolScript reads the patch script out of the embedded catalog
// asset exactly as the agent unpacks it in the container.
func extractSleepPoolScript(t *testing.T) []byte {
	t.Helper()
	raw, err := embeddedCatalog.ReadFile(sleepPoolModelDir + "/" + sleepPoolAsset)
	if err != nil {
		t.Fatalf("read embedded sleep-pool asset: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open sleep-pool asset: %v", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read sleep-pool asset: %v", err)
		}
		if header.Name == sleepPoolMember && header.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read sleep-pool patch script: %v", err)
			}
			return content
		}
	}
	t.Fatalf("embedded sleep-pool asset lacks %s", sleepPoolMember)
	return nil
}

// copySleepPoolFixture replicates the pinned image sources into a writable
// tree laid out like the vllm_ascend package inside the engine image.
func copySleepPoolFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, relative := range []string{"worker/worker.py", "worker/model_runner_v1.py"} {
		data, err := os.ReadFile(filepath.Join(sleepPoolFixtureRoot, relative))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runSleepPoolPatcher(t *testing.T, python, script, root string) (string, error) {
	t.Helper()
	output, err := exec.Command(python, "-B", sleepPoolHarness, "patch", script, root).CombinedOutput()
	return string(output), err
}

// TestAscendSleepPoolFixturesExhibitDefect proves the digest-pinned fixtures
// really are the vulnerable image sources: the whole KV cache initialization
// runs inside the discardable pool. This is the red half of the fix. If a
// future image revision is pinned and these fixtures are replaced, this test
// fails and forces a review of whether the patch is still needed.
func TestAscendSleepPoolFixturesExhibitDefect(t *testing.T) {
	python := sleepPoolPython(t)
	assertScope(t, reportScope(t, python, sleepPoolFixtureRoot), defectScope, "pristine image sources")
}

// TestAscendSleepPoolPatchNarrowsPoolScope applies the shipped catalog patch to
// those sources and asserts the pool ends up around the backing allocation
// only, that a container restart re-runs it as a no-op, and that drifted or
// hand-edited sources are refused instead of silently half-patched.
func TestAscendSleepPoolPatchNarrowsPoolScope(t *testing.T) {
	python := sleepPoolPython(t)
	script := filepath.Join(t.TempDir(), sleepPoolMember)
	if err := os.WriteFile(script, extractSleepPoolScript(t), 0o644); err != nil {
		t.Fatal(err)
	}
	root := copySleepPoolFixture(t)
	if output, err := runSleepPoolPatcher(t, python, script, root); err != nil {
		t.Fatalf("patch rejected the pinned image sources: %v\n%s", err, output)
	}
	assertScope(t, reportScope(t, python, root), fixedScope, "patched sources")

	t.Run("restart-idempotent", func(t *testing.T) {
		// The pre-start wrapper runs on every container start and the patched
		// files survive a restart: a second run must be a no-op success.
		if output, err := runSleepPoolPatcher(t, python, script, root); err != nil {
			t.Fatalf("second patch run (container restart) failed: %v\n%s", err, output)
		}
		assertScope(t, reportScope(t, python, root), fixedScope, "patched sources after restart")
	})

	t.Run("rejects-drifted-source", func(t *testing.T) {
		drifted := copySleepPoolFixture(t)
		target := filepath.Join(drifted, "worker/model_runner_v1.py")
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, append(data, "\n# upstream drifted\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
		if output, err := runSleepPoolPatcher(t, python, script, drifted); err == nil {
			t.Fatalf("patch accepted a drifted source digest:\n%s", output)
		}
	})

	t.Run("rejects-tampered-patched-source", func(t *testing.T) {
		target := filepath.Join(root, "worker/worker.py")
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, append(data, "# hand edit\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
		if output, err := runSleepPoolPatcher(t, python, script, root); err == nil {
			t.Fatalf("patch accepted a tampered patched source:\n%s", output)
		}
	})

	t.Run("missing-file-fails", func(t *testing.T) {
		incomplete := copySleepPoolFixture(t)
		if err := os.Remove(filepath.Join(incomplete, "worker/worker.py")); err != nil {
			t.Fatal(err)
		}
		if output, err := runSleepPoolPatcher(t, python, script, incomplete); err == nil {
			t.Fatalf("patch accepted a tree without worker.py:\n%s", output)
		}
	})
}

// TestAscendSleepPoolAssetMatchesBuilderSource keeps the shipped asset and the
// reviewable source in build/inference-templates/flash-next-sleep-pool in step:
// editing the script without rebuilding the asset would ship the old patch.
func TestAscendSleepPoolAssetMatchesBuilderSource(t *testing.T) {
	source, err := os.ReadFile(sleepPoolBuilderSource)
	if err != nil {
		t.Fatalf("read builder source: %v", err)
	}
	if packed := extractSleepPoolScript(t); !bytes.Equal(packed, source) {
		t.Errorf("embedded asset script differs from %s; rerun build/inference-templates/flash-next-sleep-pool/build.py",
			sleepPoolBuilderSource)
	}
}

// TestFlashNextSpecShipsSleepPoolPatch wires the fix into the product: only the
// 910C (A3) spec runs on the image whose sources the patch pins, so it is the
// only spec that may mount and run it. Every other spec of this model runs a
// different engine build, where the digest-pinned patch would hard-fail the
// container start; adding it there must be a deliberate change to this test.
func TestFlashNextSpecShipsSleepPoolPatch(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(embeddedCatalog, sleepPoolModelDir+"/"+sleepPoolAsset)
	if err != nil {
		t.Fatalf("embedded asset: %v", err)
	}
	assetDigest := fmt.Sprintf("%x", sha256.Sum256(raw))
	scriptRef := sleepPoolUnpackPath + "/" + sleepPoolMember
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != sleepPoolModelID {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != sleepPoolSpecName {
				if len(spec.Assets) != 0 || spec.PreStartScript != "" {
					t.Errorf("spec %s carries assets=%+v pre_start_script=%q; the sleep-pool patch is pinned to the A3 image only",
						spec.Name, spec.Assets, spec.PreStartScript)
				}
				continue
			}
			found = true
			if len(spec.Assets) != 1 || spec.Assets[0].File != sleepPoolAsset || spec.Assets[0].UnpackTo != sleepPoolUnpackPath {
				t.Errorf("spec %s assets=%+v, want the sleep-pool patch unpacked to %s", spec.Name, spec.Assets, sleepPoolUnpackPath)
				continue
			}
			if spec.Assets[0].SHA256 != assetDigest {
				t.Errorf("spec %s asset sha256 %s does not match the embedded file %s", spec.Name, spec.Assets[0].SHA256, assetDigest)
			}
			if spec.Assets[0].SizeBytes != int64(len(raw)) {
				t.Errorf("spec %s asset size_bytes %d, want %d", spec.Name, spec.Assets[0].SizeBytes, len(raw))
			}
			if !strings.Contains(spec.PreStartScript, "python3 "+scriptRef) {
				t.Errorf("spec %s pre_start_script %q does not run %s", spec.Name, spec.PreStartScript, scriptRef)
			}
		}
	}
	if !found {
		t.Fatalf("spec %s is gone from %s: decide explicitly what happens to the sleep-pool patch", sleepPoolSpecName, sleepPoolModelID)
	}
}
