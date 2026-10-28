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

// The Voxtral realtime catalog spec ships a pre-start patch that fixes three
// upstream vLLM defects: the end-of-stream partial frame (and with it the
// tail words of an utterance without trailing silence) is dropped, a
// final-only input_audio_buffer.commit never starts generation so no
// transcription.done is ever sent, and connection/stream teardown cancels
// tasks without awaiting them. The fixtures under testdata/voxtral-realtime
// are the verbatim upstream sources of the two supported vLLM releases
// (Apache-2.0, see LICENSE.txt there); their SHA-256 digests are pinned by
// the patch script itself, which refuses anything else.
//
// The behavior harness (testdata/voxtral-realtime/harness.py) executes the
// extracted upstream classes with stubbed dependencies and drives the defect
// scenarios. It is stdlib-only so the suite stays hermetic.

const (
	voxtralModelDir        = "catalog/models/mistralai/voxtral-mini-4b-realtime-2602"
	voxtralPatchAsset      = "assets/voxtral-realtime-patch.tar.gz"
	voxtralPatchMember     = "voxtral_realtime_patch.py"
	voxtralPatchUnpackPath = "/opt/gpuworks/voxtral-realtime-patch"
	voxtralFixtureRoot     = "testdata/voxtral-realtime"
	voxtralHarness         = "testdata/voxtral-realtime/harness.py"
)

var voxtralFixtureVersions = []string{"v0.23.0", "v0.30.0"}

// pristineDefects pins the defect each scenario must report against the
// unpatched upstream fixtures. A scenario missing here must pass on pristine
// code (these guard behaviors the patch must not regress). If upstream fixes
// a defect and the fixtures are updated, this table fails and forces a review
// of the patch's continued necessity.
var pristineDefects = map[string]string{
	"buffer/partial-frame-tail":                   "tail-frame-dropped",
	"buffer/step-aligned-tail":                    "tail-frame-dropped",
	"buffer/multi-chunk-tail":                     "tail-frame-dropped",
	"connection/final-commit-only":                "missing-completion-event",
	"connection/empty-final-commit":               "missing-completion-event",
	"connection/peer-disconnect-mid-generation":   "cleanup-does-not-await-generation",
	"connection/cancel-connection-mid-generation": "cleanup-does-not-await-generation",
	"stream/stream-flush-tail":                    "tail-frame-dropped",
	"stream/stream-empty-commit":                  "missing-final-frame",
	"stream/stream-cancel-mid-frame":              "cleanup-does-not-await-tasks",
}

func voxtralPython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip("python3 not available: Voxtral realtime patch behavior harness cannot run")
	}
	return python
}

// runHarness executes one harness mode against one source file and returns the
// per-scenario outcome: name -> defect id ("" when the scenario passed).
func runHarness(t *testing.T, python, mode, source string) map[string]string {
	t.Helper()
	output, err := exec.Command(python, "-B", voxtralHarness, mode, source).CombinedOutput()
	outcomes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "OK":
			outcomes[fields[1]] = ""
		case len(fields) >= 3 && fields[0] == "DEFECT":
			outcomes[fields[1]] = strings.TrimSuffix(fields[2], ":")
		}
	}
	if len(outcomes) == 0 {
		t.Fatalf("harness %s %s produced no scenario results: %v\n%s", mode, source, err, output)
	}
	return outcomes
}

func voxtralFixtureFile(version, relative string) string {
	return filepath.Join(voxtralFixtureRoot, version, relative)
}

const (
	voxtralConnectionFile = "entrypoints/speech_to_text/realtime/connection.py"
	voxtralModelFile      = "model_executor/models/voxtral_realtime.py"
)

func runVoxtralHarnessSuite(t *testing.T, python, version, modelSource, connectionSource string) map[string]string {
	t.Helper()
	merged := map[string]string{}
	for _, group := range []struct{ mode, source string }{
		{"buffer", modelSource},
		{"stream", modelSource},
		{"connection", connectionSource},
	} {
		for name, defect := range runHarness(t, python, group.mode, group.source) {
			merged[name] = defect
		}
	}
	return merged
}

// TestVoxtralRealtimeUpstreamFixturesExhibitReportedDefects proves the
// digest-pinned fixtures really are the vulnerable upstream code: every
// reported defect reproduces, and the guard scenarios already behave
// correctly. This is the red half of the fix; the patch test below is green.
func TestVoxtralRealtimeUpstreamFixturesExhibitReportedDefects(t *testing.T) {
	python := voxtralPython(t)
	for _, version := range voxtralFixtureVersions {
		t.Run(version, func(t *testing.T) {
			outcomes := runVoxtralHarnessSuite(t, python, version,
				voxtralFixtureFile(version, voxtralModelFile),
				voxtralFixtureFile(version, voxtralConnectionFile))
			for name, defect := range outcomes {
				expected, vulnerable := pristineDefects[name]
				switch {
				case vulnerable && defect == "":
					t.Errorf("%s %s: scenario unexpectedly passes on pristine upstream code; fixtures may no longer be the vulnerable sources", version, name)
				case vulnerable && defect != expected:
					t.Errorf("%s %s: pristine defect is %q, want %q", version, name, defect, expected)
				case !vulnerable && defect != "":
					t.Errorf("%s %s: guard scenario fails on pristine upstream code: %q", version, name, defect)
				}
			}
			for name := range pristineDefects {
				if _, ran := outcomes[name]; !ran {
					t.Errorf("%s %s: harness did not run this scenario", version, name)
				}
			}
		})
	}
}

// extractVoxtralPatchScript reads the patch script out of the embedded
// catalog asset exactly as the agent would receive it.
func extractVoxtralPatchScript(t *testing.T) []byte {
	t.Helper()
	raw, err := embeddedCatalog.ReadFile(voxtralModelDir + "/" + voxtralPatchAsset)
	if err != nil {
		t.Fatalf("read embedded Voxtral patch asset: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open Voxtral patch asset: %v", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read Voxtral patch asset: %v", err)
		}
		if header.Name == voxtralPatchMember && header.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read Voxtral patch script: %v", err)
			}
			return content
		}
	}
	t.Fatalf("embedded Voxtral patch asset lacks %s", voxtralPatchMember)
	return nil
}

// copyVoxtralFixture replicates one fixture version into a writable tree laid
// out like the vllm package inside the engine image.
func copyVoxtralFixture(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(voxtralFixtureRoot, version)
	for _, relative := range []string{voxtralConnectionFile, voxtralModelFile} {
		data, err := os.ReadFile(filepath.Join(source, relative))
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

func runVoxtralPatcher(t *testing.T, python, script, root string) (string, error) {
	t.Helper()
	output, err := exec.Command(python, "-B", voxtralHarness, "patch", script, root).CombinedOutput()
	return string(output), err
}

// TestVoxtralRealtimePatchRepairsDefects applies the shipped catalog patch to
// both supported upstream releases and asserts every defect scenario passes,
// the patch is idempotent across container restarts, and tampered sources are
// refused instead of silently half-patched.
func TestVoxtralRealtimePatchRepairsDefects(t *testing.T) {
	python := voxtralPython(t)
	script := filepath.Join(t.TempDir(), voxtralPatchMember)
	if err := os.WriteFile(script, extractVoxtralPatchScript(t), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, version := range voxtralFixtureVersions {
		t.Run(version, func(t *testing.T) {
			root := copyVoxtralFixture(t, version)
			if output, err := runVoxtralPatcher(t, python, script, root); err != nil {
				t.Fatalf("patch rejected supported %s sources: %v\n%s", version, err, output)
			}
			outcomes := runVoxtralHarnessSuite(t, python, version,
				filepath.Join(root, voxtralModelFile),
				filepath.Join(root, voxtralConnectionFile))
			for name, defect := range outcomes {
				if defect != "" {
					t.Errorf("%s %s: patched sources still defective: %q", version, name, defect)
				}
			}

			t.Run("restart-idempotent", func(t *testing.T) {
				// The pre-start wrapper runs on every container start, and the
				// container filesystem keeps the patched site-packages: a
				// second run must be a no-op success, not a failure.
				if output, err := runVoxtralPatcher(t, python, script, root); err != nil {
					t.Fatalf("second patch run (container restart) failed: %v\n%s", err, output)
				}
			})

			t.Run("rejects-unknown-source", func(t *testing.T) {
				tampered := copyVoxtralFixture(t, version)
				target := filepath.Join(tampered, voxtralModelFile)
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, append(data, "\n# upstream drifted\n"...), 0o644); err != nil {
					t.Fatal(err)
				}
				if output, err := runVoxtralPatcher(t, python, script, tampered); err == nil {
					t.Fatalf("patch accepted a drifted source digest:\n%s", output)
				}
			})

			t.Run("rejects-tampered-patched-source", func(t *testing.T) {
				target := filepath.Join(root, voxtralModelFile)
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, append(data, "# hand edit\n"...), 0o644); err != nil {
					t.Fatal(err)
				}
				if output, err := runVoxtralPatcher(t, python, script, root); err == nil {
					t.Fatalf("patch accepted a tampered patched source:\n%s", output)
				}
			})
		})
	}
}

// TestVoxtralRealtimeSpecShipsPatch wires the fix into the product: the
// upstream-based vLLM spec must mount the patch asset and run it before the
// engine starts, so deployments get the corrected realtime endpoint without
// an image rebuild. The PPU spec runs a vendor vLLM build whose sources are
// not digest-verified; the digest-pinned patch would hard-fail its container
// start on any drift, so it deliberately ships without the patch until the
// image's vLLM files are verified — re-enabling it must be a deliberate
// change to this test.
func TestVoxtralRealtimeSpecShipsPatch(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(embeddedCatalog, voxtralModelDir+"/"+voxtralPatchAsset)
	if err != nil {
		t.Fatalf("embedded asset: %v", err)
	}
	assetDigest := fmt.Sprintf("%x", sha256.Sum256(raw))
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		if model.ModelID != "mistralai/voxtral-mini-4b-realtime-2602" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Engine != "vllm" {
				continue
			}
			seen[spec.Name] = true
			switch spec.Name {
			case "bf16-vllm-nvidia-general":
				if len(spec.Assets) != 1 || spec.Assets[0].File != voxtralPatchAsset || spec.Assets[0].UnpackTo != voxtralPatchUnpackPath {
					t.Errorf("spec %s assets=%+v, want the realtime patch asset unpacked to %s", spec.Name, spec.Assets, voxtralPatchUnpackPath)
					continue
				}
				scriptRef := voxtralPatchUnpackPath + "/" + voxtralPatchMember
				if !strings.Contains(spec.PreStartScript, "python3 "+scriptRef) {
					t.Errorf("spec %s pre_start_script %q does not run %s", spec.Name, spec.PreStartScript, scriptRef)
				}
				if spec.Assets[0].SHA256 != assetDigest {
					t.Errorf("spec %s asset sha256 %s does not match embedded file %s", spec.Name, spec.Assets[0].SHA256, assetDigest)
				}
			case "bf16-vllm-ppu-2769294d":
				if len(spec.Assets) != 0 {
					t.Errorf("spec %s assets=%+v, want none: the vendor vLLM build is not digest-verified, the pinned patch must not be wired until it is", spec.Name, spec.Assets)
				}
				if spec.PreStartScript != "" {
					t.Errorf("spec %s pre_start_script %q, want empty: the realtime patch must not run on the unverified vendor vLLM build", spec.Name, spec.PreStartScript)
				}
			default:
				t.Errorf("unexpected vLLM spec %s on the Voxtral realtime model: decide explicitly whether it ships the realtime patch", spec.Name)
			}
		}
	}
	for _, name := range []string{"bf16-vllm-nvidia-general", "bf16-vllm-ppu-2769294d"} {
		if !seen[name] {
			t.Errorf("mistralai/voxtral-mini-4b-realtime-2602 lost its %s spec", name)
		}
	}
}
