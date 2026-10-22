package app

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The assistant must see the command an instance actually runs, read from the
// desired-state snapshot it was created with: the diagnosis seed and the
// healthy-replica comparison both quote it.
func TestInstanceLaunchCommandReadsTheDesiredStateSnapshot(t *testing.T) {
	desired := proto.InstanceDesiredState{
		ResolvedLaunch: proto.ResolvedLaunch{
			CommandMode: string(deploy.CommandManual),
			Entrypoint:  []string{"/bin/sh"},
			Argv:        []string{"-c", "set -a\nvllm serve /model --port 30000"},
		},
		UUID:         "ai-spec-instance",
		DesiredState: "running",
	}
	raw, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	command, mode := instanceLaunchCommand(store.Instance{UUID: desired.UUID, DesiredStateJSON: string(raw)})
	want := []string{"/bin/sh", "-c", "set -a\nvllm serve /model --port 30000"}
	if !reflect.DeepEqual(command, want) || mode != "manual" {
		t.Fatalf("command = %#v mode = %q, want %#v manual", command, mode, want)
	}
}

// A desired state that cannot be parsed degrades to no command rather than to
// invented context.
func TestInstanceLaunchCommandUnreadableSnapshotYieldsNoCommand(t *testing.T) {
	for _, snapshot := range []string{`{not json`, ``} {
		if command, _ := instanceLaunchCommand(store.Instance{UUID: "broken", DesiredStateJSON: snapshot}); len(command) > 0 {
			t.Fatalf("snapshot %q: command = %#v, want none", snapshot, command)
		}
	}
}
