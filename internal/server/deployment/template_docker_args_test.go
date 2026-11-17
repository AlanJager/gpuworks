package deployment

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A spec's docker_args travels in the deployment's snapshot the same way
// memlock_unlimited does, and a deployment without a template states none.
func TestTemplateSnapshotKeepsTheSpecDockerArgs(t *testing.T) {
	selection := templateSelection{
		Model:      &store.InferenceModel{ModelID: "acme/docker-args"},
		Spec:       &store.InferenceSpecRow{Name: "privileged-recipe", Source: "upstream"},
		DockerArgs: []string{"--cap-add=SYS_PTRACE", "--security-opt=seccomp=unconfined"},
	}
	snapshot := snapshotTemplateSelection(selection, "img:1")
	if !reflect.DeepEqual(snapshot.DockerArgs, selection.DockerArgs) || !reflect.DeepEqual(selection.dockerArgs(), selection.DockerArgs) {
		t.Fatalf("snapshot lost docker_args: %+v", snapshot)
	}
	if (templateSelection{}).dockerArgs() != nil {
		t.Fatal("a deployment without a template states docker args")
	}
	selection.DockerArgs = nil
	if got := snapshotTemplateSelection(selection, "img:1").DockerArgs; got != nil {
		t.Fatalf("a spec stating none snapshots as %v", got)
	}
}
