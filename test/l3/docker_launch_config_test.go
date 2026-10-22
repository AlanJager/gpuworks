//go:build docker

package l3

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ADR 0020 made the launch configuration explicit: what a deployment states is
// emitted, what it does not state is not, and dictionary-owned flags a catalog
// spec wrote as argv are split into fields and re-emitted from there. Both
// change the command line a container actually receives, so they are only
// really proven against a real container runtime.

// TestDockerFakeEngineStartsWithMaterializedArgv deploys a form that states the
// utilization and the boolean switches, checks each reaches the container
// exactly once, and checks that a second deployment stating none of them
// carries none of them. The risk this covers is the one ADR 0020 names: a flag
// an engine chokes on fails at startup rather than in any unit test, and the
// upgrade guarantee is that an untouched deployment gains no flags at all.
func TestDockerFakeEngineStartsWithMaterializedArgv(t *testing.T) {
	fixture := newV4DockerFixture(t, "materialized-argv")
	depID, instance := fixture.createDeployment(t, map[string]any{
		"image": fixture.imageRef, "served_name": "l3-materialized-argv",
		"quick_params": map[string]any{
			"gpu_memory_utilization": 0.9,
			"enable_prefix_caching":  true,
			"enable_chunked_prefill": true,
			"enable_cache_report":    true,
		},
	})
	argv := containerArgv(t, instance.ContainerID)
	if got := launchArgValues(argv, "--gpu-memory-utilization"); len(got) != 1 || got[0] != "0.9" {
		t.Errorf("--gpu-memory-utilization=%v, want exactly one 0.9 as stated; argv=%v", got, argv)
	}
	for _, flag := range []string{"--enable-prefix-caching", "--enable-chunked-prefill", "--enable-prompt-tokens-details"} {
		if got := launchArgValues(argv, flag); len(got) != 1 {
			t.Errorf("%s appears %d times, want once: a boolean stated true is emitted in its on form; argv=%v", flag, len(got), argv)
		}
	}
	// Nothing was said about dtype or the KV cache dtype, so nothing is said to
	// the engine either.
	for _, flag := range []string{"--dtype", "--kv-cache-dtype"} {
		if got := launchArgValues(argv, flag); len(got) != 0 {
			t.Errorf("%s=%v, want absent: nobody stated it; argv=%v", flag, got, argv)
		}
	}
	// Reaching this point already means the container passed its health check
	// (the fixture waits for the running state). Answering a request through
	// the gateway proves the process behind it is really serving.
	key, err := fixture.env.Store().CreateAPIKey("l3-materialized-argv", 0)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, fixture.env.URL()+"/v1/chat/completions",
		strings.NewReader(`{"model":"l3-materialized-argv","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "choices") {
		t.Fatalf("gateway status=%d body=%s: the explicit flags broke the served endpoint", response.StatusCode, body)
	}
	fixture.deleteDeployment(t, depID)
	// The fixture node has a single GPU and a delete only starts the teardown.
	// Creating the next deployment while the first instance still claims the
	// card answers no_free_gpu and lands on the controller's next retry, past the
	// fixture's deadline, so wait for the claim to be gone first.
	fixture.env.Eventually(t, 45*time.Second, func() bool {
		instances, _ := fixture.env.Store().InstancesForDeployment(depID)
		return len(instances) == 0
	})

	// The upgrade guarantee, against a real runtime: a deployment that states
	// nothing receives the dictionary's own defaults - the payload the deploy
	// form's controls always showed - and no flag beyond them: the enum
	// defaults are no-ops the adapter does not spell, and rows without a
	// default stay silent.
	bareID, bareInstance := fixture.createDeployment(t, map[string]any{
		"image": fixture.imageRef, "served_name": "l3-materialized-argv-bare",
		"quick_params": map[string]any{},
	})
	bareArgv := containerArgv(t, bareInstance.ContainerID)
	for _, flag := range []string{"--enable-prefix-caching", "--enable-chunked-prefill", "--dtype", "--kv-cache-dtype"} {
		if got := launchArgValues(bareArgv, flag); len(got) != 0 {
			t.Errorf("a deployment that stated nothing carries %s=%v; argv=%v", flag, got, bareArgv)
		}
	}
	for _, flag := range []string{"--gpu-memory-utilization", "--enable-prompt-tokens-details"} {
		if got := launchArgValues(bareArgv, flag); len(got) != 1 {
			t.Errorf("a deployment that stated nothing must carry %s exactly once; argv=%v", flag, bareArgv)
		}
	}
	fixture.deleteDeployment(t, bareID)
}

// TestDockerTemplateArgsSplitReachesContainer deploys a catalog spec whose args
// mix dictionary-owned flags (one written as an alias) with a flag the
// dictionary does not know. The spec is deployed untouched, so the command the
// container receives must hold the canonical form of each known flag exactly
// once and the unknown token verbatim, and the deployment must not be recorded
// as having deviated from its template.
//
// TestDockerInferenceTemplateDeployPropagatesProfileArgsAndEnv already proves a
// spec's args and env reach the container at all (with --trust-remote-code).
// What is new here is the split itself: an alias that must be canonicalized on
// the way through a field, a value flag, and a residual that must survive
// untouched - none of which that case can show.
func TestDockerTemplateArgsSplitReachesContainer(t *testing.T) {
	fixture := newV4DockerFixture(t, "template-args-split")
	const specName = "half-fp8"
	// The weight source's revision must match the fixture model's exactly, or
	// the template-owned deploy resolves a different, file-less model row.
	catalogModel := store.InferenceModel{
		ModelID: fixture.model.SourceID, DisplayTitle: "L3 template args split", Source: "upstream",
		Specs: []store.InferenceSpecRow{{
			Name: specName, Scenario: "balanced", Engine: "vllm", Image: fixture.imageRef, Quantization: "bf16",
			WeightSources: `[{"provider":"modelscope","repo_id":"` + fixture.model.SourceID + `","revision":"` + fixture.model.Revision + `"}]`,
			// Vendor alone. A compute-capability predicate would force the test
			// to write a capability into the GPU row behind the agent's back,
			// and the node's next status report puts the fixture's empty value
			// straight back - a race that has nothing to do with what is being
			// proven here.
			GPUs: `{"vendor":"nvidia"}`,
			Args: `["--dtype","half","--kv-cache-dtype","fp8","--no-enable-flashinfer-autotune"]`,
			Env:  `{}`, PreStartArgv: `[]`,
		}},
	}
	if err := fixture.env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalogModel}); err != nil {
		t.Fatal(err)
	}
	graph, err := fixture.env.Store().InferenceModelByModelID(catalogModel.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	depID, instance := fixture.createDeployment(t, map[string]any{
		"template_id": graph.ID, "template_spec_name": specName, "scenario": "balanced",
		"served_name": "l3-template-args-split",
	})
	argv := containerArgv(t, instance.ContainerID)
	if got := launchArgValues(argv, "--dtype"); len(got) != 1 || got[0] != "float16" {
		t.Errorf("--dtype=%v, want exactly one float16: the spec's alias \"half\" is canonicalized through the field; argv=%v", got, argv)
	}
	if got := launchArgValues(argv, "--kv-cache-dtype"); len(got) != 1 || got[0] != "fp8" {
		t.Errorf("--kv-cache-dtype=%v, want exactly one fp8; argv=%v", got, argv)
	}
	if got := launchArgValues(argv, "--no-enable-flashinfer-autotune"); len(got) != 1 {
		t.Errorf("--no-enable-flashinfer-autotune appears %d times, want once verbatim: a flag the dictionary does not know passes through unchanged; argv=%v", len(got), argv)
	}
	// The alias is the case a text comparison gets wrong: "half" in the spec and
	// "float16" in the configuration are the same value, so an untouched deploy
	// is not a deviation.
	dep, err := fixture.env.Store().GetDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	if dep.TemplateDeviated {
		t.Errorf("an untouched template deploy is marked as deviated: %+v", dep)
	}
	if dep.TemplateID != graph.ID || dep.TemplateSpecName != specName {
		t.Errorf("deployment provenance=%+v, want template %d/%s", dep, graph.ID, specName)
	}
	fixture.deleteDeployment(t, depID)
}

// launchArgValues returns one entry per occurrence of flag in argv, carrying the
// value that followed it (empty for a bare flag). Counting occurrences is the
// point: the failure ADR 0020 removes is the same flag arriving twice from two
// different layers.
func launchArgValues(argv []string, flag string) []string {
	var values []string
	for index, token := range argv {
		switch {
		case token == flag:
			value := ""
			if index+1 < len(argv) && !strings.HasPrefix(argv[index+1], "--") {
				value = argv[index+1]
			}
			values = append(values, value)
		case strings.HasPrefix(token, flag+"="):
			values = append(values, strings.TrimPrefix(token, flag+"="))
		}
	}
	return values
}
