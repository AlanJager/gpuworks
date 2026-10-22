package templateprobe

import (
	"encoding/json"
	"os"
	"testing"
)

type signatureFixture struct {
	CaseID  string `json:"case_id"`
	Status  string `json:"status"`
	Forward bool   `json:"forward"`
	Log     string `json:"log"`
}

func TestK100First38FailureSignatureRegression(t *testing.T) {
	body, err := os.ReadFile("testdata/k100-first38-signatures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []signatureFixture
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 38 {
		t.Fatalf("fixture count = %d, want exact acceptance subset of 38", len(fixtures))
	}
	pass, fail, baseCaught, forwardCaught := 0, 0, 0, 0
	for _, fixture := range fixtures {
		base := ClassifyFailure(fixture.Log, ClassifyOptions{})
		withForward := ClassifyFailure(fixture.Log, ClassifyOptions{IncludeForward: true})
		if fixture.Status == "PASS" {
			pass++
			if base.Matched || withForward.Matched {
				t.Errorf("PASS %s was misclassified: base=%+v forward=%+v", fixture.CaseID, base, withForward)
			}
			continue
		}
		fail++
		if base.Matched {
			baseCaught++
		}
		if withForward.Matched {
			forwardCaught++
		}
	}
	if pass != 18 || fail != 20 {
		t.Fatalf("dataset accounting = %d PASS + %d FAIL, want 18 + 20", pass, fail)
	}
	if baseCaught != 13 || forwardCaught != 14 {
		t.Fatalf("caught = %d base / %d with forward, want 13 / 14", baseCaught, forwardCaught)
	}
}

// The nano_v3 defect as it reached the probe: the catalog declared
// --reasoning-parser-plugin nano_v3_reasoning_parser.py, the file was in no
// image, in no weights repository and in no asset, and the engine died while
// importing before it loaded a single weight.
func TestClassifierAttributesMissingLaunchFileToTheCatalog(t *testing.T) {
	const engineLog = `Process SpawnProcess-1:
Traceback (most recent call last):
  File "/usr/local/lib/python3.12/site-packages/vllm/utils/import_utils.py", line 1188, in import_from_path
    module = _load_module(name, path)
  File "/usr/local/lib/python3.12/site-packages/vllm/utils/import_utils.py", line 1055, in _load_module
    spec.loader.exec_module(module)
FileNotFoundError: [Errno 2] No such file or directory: '/nano_v3_reasoning_parser.py'`
	match := ClassifyFailure(engineLog, ClassifyOptions{})
	if !match.Matched || match.Family != FailureCatalogAsset || match.SignatureID != "catalog.asset.file_missing" {
		t.Fatalf("engine traceback classified as %+v", match)
	}
	if !match.Deterministic || !match.Excludes {
		t.Fatalf("missing launch file must be a deterministic exclusion: %+v", match)
	}
	// The launcher form, for a container that never starts Python.
	const launcherLog = "python: can't open file '/nemotron_toolcall_parser_no_streaming.py': [Errno 2] No such file or directory"
	if got := ClassifyFailure(launcherLog, ClassifyOptions{}); got.Family != FailureCatalogAsset {
		t.Fatalf("launcher form classified as %+v", got)
	}
	// The family must not absorb a bare phrase: a missing optional path is not
	// a catalog declaration the deployment failed to supply.
	for _, log := range []string{
		"model_load=PASS health=200 chat=200",
		"warning: optional chat_template.json not found; no such file or directory is tolerated",
	} {
		if got := ClassifyFailure(log, ClassifyOptions{IncludeForward: true}); got.Matched {
			t.Errorf("ClassifyFailure(%q) = %+v, want inconclusive", log, got)
		}
	}
}

func TestClassifierDoesNotPromoteNondeterministicFailures(t *testing.T) {
	for _, log := range []string{
		"CUDA out of memory",
		"health check timed out",
		"semantic_output_validation=FAIL output=!!!!!!!!",
		"ASR harness invocation failed",
		"cleanup guard found an orphaned namespace",
	} {
		if got := ClassifyFailure(log, ClassifyOptions{IncludeForward: true}); got.Matched {
			t.Errorf("ClassifyFailure(%q) = %+v, want inconclusive", log, got)
		}
	}
}
