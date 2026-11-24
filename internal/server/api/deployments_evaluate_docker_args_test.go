package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A docker args list pasted from a docker run command: every refused arg is
// a field error beside the docker args field, all at once, and none lands on
// another control (an uncoded refusal used to start on extra_args, leaving the
// docker args field without a message). The host-access options are allowed
// and come back as warnings.
func TestEvaluateDockerArgsRefusalsLandOnTheDockerArgsField(t *testing.T) {
	st, err := store.Open("file:evaluate_docker_args_field?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/docker-args", Revision: "main", Name: "docker-args", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	pasted := `["--shm-size 256g","--network=host ","--name Qwen3.8_INT","--privileged","--device=/dev/kfd","--device=/dev/dri","--device=/dev/mkfd","--group-add video ","--cap-add=SYS_PTRACE","--security-opt seccomp=unconfined","--pid=host"]`
	status, raw := evaluateForFieldErrors(t, h, fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"docker_args":%s}}`, model.ID, pasted))
	if status != http.StatusOK || string(raw["ok"]) != "false" {
		t.Fatalf("status = %d ok = %s", status, raw["ok"])
	}
	errors := fieldErrorsOf(t, raw)
	want := []struct{ code, arg string }{
		{ErrorDockerArgUseShmField, "--shm-size 256g"},
		{ErrorDockerArgPlatformManaged, "--network=host"},
		{ErrorDockerArgPlatformManaged, "--name Qwen3.8_INT"},
	}
	if len(errors) != len(want) {
		t.Fatalf("errors = %+v, want %d docker args errors", errors, len(want))
	}
	for i, w := range want {
		got := errors[i]
		if got.Field != deployment.FieldContainerDocker || got.Code != w.code || got.Params["arg"] != w.arg {
			t.Fatalf("errors[%d] = %+v, want %s for %q on %s", i, got, w.code, w.arg, deployment.FieldContainerDocker)
		}
	}
	assertHostAccessWarnings(t, raw, "--privileged", "--pid=host")

	// Once the refused args are gone the draft evaluates, still warned.
	status, raw = evaluateForFieldErrors(t, h, fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","container_overrides":{"docker_args":["--privileged","--pid","host","--cap-add=SYS_PTRACE"]}}`, model.ID))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if errors := fieldErrorsOf(t, raw); len(errors) != 0 {
		t.Fatalf("errors = %+v, want none: host-access options are allowed", errors)
	}
	assertHostAccessWarnings(t, raw, "--privileged", "--pid")
}

func assertHostAccessWarnings(t *testing.T, raw map[string]json.RawMessage, args ...string) {
	t.Helper()
	var warnings []messages.Warning
	if err := json.Unmarshal(raw["warnings"], &warnings); err != nil {
		t.Fatalf("warnings %s: %v", raw["warnings"], err)
	}
	var got []string
	for _, w := range warnings {
		if w.Code == messages.WarningDockerArgPrivileged || w.Code == messages.WarningDockerArgSharedPID {
			got = append(got, fmt.Sprint(w.Params["arg"]))
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(args) {
		t.Fatalf("host access warnings = %q, want %q (all: %s)", got, args, raw["warnings"])
	}
}
