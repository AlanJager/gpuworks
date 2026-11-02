package seed

import (
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// hygonSGLangPageSize is the KV page size the Hygon DTK SGLang build can run
// with. Its flash-attention kernel (flash_attn_interface.vllm_mha_varlen_fwd)
// only accepts 64-token paged KV blocks, and SGLang's default page size of 1
// passes weight loading, graph capture and the health check, then dies on the
// first forward_extend with "Paged KV cache block size must be 64" and a
// VMFault. The backend is picked automatically on DCU when
// --attention-backend is absent, so the requirement is the engine build's, not
// something a spec opts into by naming fa3.
const hygonSGLangPageSize = "64"

// isHygonSGLangSpec reports whether the spec runs SGLang on DTK. The vendor
// predicate is what the scheduler matches on; the image check also catches a
// spec whose vendor was mistyped, which would otherwise slip out of this guard
// while still launching the DTK build.
func isHygonSGLangSpec(spec inferencecatalog.InferenceSpec) bool {
	if spec.Engine != "sglang" {
		return false
	}
	return spec.GPUs.Vendor == accelerator.VendorHygon || strings.Contains(strings.ToLower(spec.Image), "dtk")
}

// hygonSGLangPageSizeProblem names what is wrong with a spec's page size, or
// returns "" when the spec pins the page size the DTK build requires.
func hygonSGLangPageSizeProblem(args []string) string {
	if count := commandFlagCount(args, "--page-size"); count != 1 {
		return "has " + strconv.Itoa(count) + " --page-size flags, want exactly one --page-size " + hygonSGLangPageSize
	}
	if got := commandFlagValue(args, "--page-size"); got != hygonSGLangPageSize {
		return "pins --page-size " + got + ", the DTK flash-attention kernel only runs with " + hygonSGLangPageSize
	}
	return ""
}

// TestEveryHygonSGLangSpecPinsTheDTKPageSize sweeps the built-in catalog by
// spec (backend and engine), not by file, because one model.yaml mixes
// vendors. The count guard keeps the sweep from going green because the
// predicate stopped matching anything.
func TestEveryHygonSGLangSpecPinsTheDTKPageSize(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	judged := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if !isHygonSGLangSpec(spec) {
				continue
			}
			judged++
			if problem := hygonSGLangPageSizeProblem(spec.Args); problem != "" {
				t.Errorf("%s/%s %s: %v", model.ModelID, spec.Name, problem, spec.Args)
			}
		}
	}
	if judged < 1 {
		t.Fatal("no Hygon SGLang spec found; the predicate no longer matches the catalog")
	}
	t.Logf("checked %d Hygon SGLang specs", judged)
}

// TestHygonSGLangPageSizeGuardRejectsAViolation proves the sweep above is not
// green because the guard is inert: dropping the flag, or pinning another
// value, is reported.
func TestHygonSGLangPageSizeGuardRejectsAViolation(t *testing.T) {
	_, spec := catalogSpec(t, "qwen/qwen3-4b", "bf16-sglang-hygon-7a8f6f7b")
	if !isHygonSGLangSpec(spec) {
		t.Fatal("the reference Hygon SGLang spec is not recognised as one")
	}
	if problem := hygonSGLangPageSizeProblem(spec.Args); problem != "" {
		t.Fatalf("reference spec is expected to pass: %s", problem)
	}
	for name, args := range map[string][]string{
		"flag dropped":     dropArgumentPair(spec.Args, "--page-size", hygonSGLangPageSize),
		"another value":    replaceArgumentValue(spec.Args, "--page-size", "1"),
		"flag given twice": append(append([]string(nil), spec.Args...), "--page-size", hygonSGLangPageSize),
	} {
		if hygonSGLangPageSizeProblem(args) == "" {
			t.Errorf("%s: guard accepted %v", name, args)
		}
	}
}

// TestHygonSGLangSpecLaunchesWithThePageSizeOnce runs the real catalog spec
// through the argv assembly the deployment uses, so the assertion is on the
// command line the container gets rather than on the YAML.
func TestHygonSGLangSpecLaunchesWithThePageSizeOnce(t *testing.T) {
	_, spec := catalogSpec(t, "qwen/qwen3-4b", "bf16-sglang-hygon-7a8f6f7b")
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	launch := func(extra []string) []string {
		t.Helper()
		resolved, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
			Engine: engine,
			Params: deploy.Params{
				ModelPath: "/model", APIPort: 30000, TensorParallel: 1,
				TemplateArgs: spec.Args, TemplateEnv: spec.Env, ExtraArgs: extra,
			},
			PlacementVariables: map[string]any{"GPU_COUNT": int64(1), "TP_SIZE": int64(1), "SP_DEGREE": int64(1)},
			APIContract:        spec.API,
		})
		if err != nil {
			t.Fatal(err)
		}
		return append(append([]string(nil), resolved.Entrypoint...), resolved.Argv...)
	}

	command := launch(nil)
	if got, count := commandFlagValue(command, "--page-size"), commandFlagCount(command, "--page-size"); got != hygonSGLangPageSize || count != 1 {
		t.Fatalf("launched --page-size %q (%d flags), want %s once: %v", got, count, hygonSGLangPageSize, command)
	}
	if got := commandFlagValue(command, "--attention-backend"); got != "fa3" {
		t.Errorf("launched --attention-backend %q, want fa3: %v", got, command)
	}

	// A page size the user sets explicitly keeps priority over the template's,
	// and the engine still sees the flag once: it keeps whichever copy it
	// parses last, so a duplicate would silently pick a side.
	overridden := launch([]string{"--page-size", "128"})
	if got, count := commandFlagValue(overridden, "--page-size"), commandFlagCount(overridden, "--page-size"); got != "128" || count != 1 {
		t.Fatalf("user --page-size 128 launched as %q (%d flags): %v", got, count, overridden)
	}
}
