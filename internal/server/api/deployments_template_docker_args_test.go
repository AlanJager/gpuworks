package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The preview is the launch the instance will get, so the runtime block the
// deploy page and the instance detail show carries the template's own
// docker_args before the deployment's own and the platform's, the same layer
// order memlock_unlimited and shm_bytes take (deploy.launchDockerArgs).
func TestEvaluatePreviewCarriesTheTemplateDockerArgs(t *testing.T) {
	st, err := store.Open("file:evaluate_template_docker_args?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/DockerArgs", Revision: "main", Name: "Acme/DockerArgs", WeightBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	spec := func(name string, dockerArgs string) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Quantization: "bf16", Image: "vllm/vllm-openai:latest", WeightBytes: 1 << 30, DockerArgs: dockerArgs,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/DockerArgs"}]`, GPUs: `{"vendor":"nvidia"}`,
		}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/docker-args", DisplayTitle: "DockerArgs", Source: "upstream", Architecture: "qwen3", ContextLength: 4096,
		Specs: []store.InferenceSpecRow{
			spec("privileged-recipe", `["--cap-add=SYS_PTRACE","--security-opt=seccomp=unconfined"]`),
			spec("plain", ""),
		},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/docker-args")
	if err != nil {
		t.Fatal(err)
	}
	body := func(specName, extra string) string {
		return fmt.Sprintf(`{"template_id":%d,"template_spec_name":%q,"node_uuid":"field-errors-node"%s}`, template.ID, specName, extra)
	}
	for name, test := range map[string]struct {
		body string
		want []string // docker_args on the wire; nil when the key is absent
	}{
		"the spec's own docker args":           {body("privileged-recipe", ""), []string{"--cap-add=SYS_PTRACE", "--security-opt=seccomp=unconfined"}},
		"a spec stating none adds nothing":     {body("plain", ""), nil},
		"the deployment's own args come after": {body("privileged-recipe", `,"container_overrides":{"docker_args":["--cap-add","SYS_NICE"]}`), []string{"--cap-add=SYS_PTRACE", "--security-opt=seccomp=unconfined", "--cap-add", "SYS_NICE"}},
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := evaluateForFieldErrors(t, h, test.body)
			if status != http.StatusOK || string(raw["ok"]) != "true" {
				t.Fatalf("status = %d body = %s", status, raw)
			}
			encoded, present := raw["docker_args"]
			if (test.want == nil) == present {
				t.Fatalf("docker_args = %s (present %v), want %v", encoded, present, test.want)
			}
			if test.want == nil {
				return
			}
			var got []string
			if err := json.Unmarshal(encoded, &got); err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("docker_args = %s (%v), want %v", encoded, err, test.want)
			}
		})
	}
}

// A deployment's own shm_bytes has no effect when the selected template's
// docker_args already put the container on the host's IPC namespace: the
// deployment did not state --ipc=host itself, so only the combined check that
// knows the template catches it.
func TestEvaluateRefusesDeploymentShmWithTemplateHostIPC(t *testing.T) {
	st, err := store.Open("file:evaluate_template_host_ipc?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/HostIPC", Revision: "main", Name: "Acme/HostIPC", WeightBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/host-ipc", DisplayTitle: "HostIPC", Source: "upstream", Architecture: "qwen3", ContextLength: 4096,
		Specs: []store.InferenceSpecRow{{
			Name: "host-ipc", Scenario: "balanced", Engine: "vllm", Quantization: "bf16", Image: "vllm/vllm-openai:latest", WeightBytes: 1 << 30,
			DockerArgs:    `["--ipc=host"]`,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/HostIPC"}]`, GPUs: `{"vendor":"nvidia"}`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/host-ipc")
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"host-ipc","node_uuid":"field-errors-node","container_overrides":{"shm_bytes":34359738368}}`, template.ID)
	status, raw := evaluateForFieldErrors(t, h, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, raw)
	}
	errs := fieldErrorsOf(t, raw)
	found := false
	for _, fe := range errs {
		if fe.Code == deploy.ErrorShmSizeWithHostIPC {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors = %+v, want %s", errs, deploy.ErrorShmSizeWithHostIPC)
	}
	// Without a stated shm_bytes the template's --ipc=host is simply in
	// effect: no conflict to report.
	plain := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"host-ipc","node_uuid":"field-errors-node"}`, template.ID)
	status, raw = evaluateForFieldErrors(t, h, plain)
	if status != http.StatusOK || string(raw["ok"]) != "true" {
		t.Fatalf("status = %d body = %s", status, raw)
	}
}
