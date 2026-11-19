package engine

import (
	"log/slog"
	"sort"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// StorageUsers answers "what may have a directory mounted": every instance
// the agent holds whatever its state, and whether that list can be trusted.
func TestStorageUsersListsInstancesInEveryState(t *testing.T) {
	e := newHermeticEngine(&headlessTestDriver{}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())
	if _, settled := e.StorageUsers(); settled {
		t.Fatal("settled before the control plane sent any instance")
	}
	e.desiredKnown = true
	if specs, settled := e.StorageUsers(); !settled || len(specs) != 0 {
		t.Fatalf("an agent told it runs nothing: specs=%v settled=%v", specs, settled)
	}
	e.desired["awake"] = proto.InstanceDesiredState{UUID: "awake", DesiredState: "running", ModelPath: "/s/a"}
	e.desired["asleep"] = proto.InstanceDesiredState{UUID: "asleep", DesiredState: "running", DesiredSleep: 2, ModelPath: "/s/b"}
	e.setSleepLevelLocked("asleep", 2)
	e.desired["stopped"] = proto.InstanceDesiredState{UUID: "stopped", DesiredState: "stopped", ModelPath: "/s/c"}
	e.desired["stopping"] = proto.InstanceDesiredState{UUID: "stopping", DesiredState: "stopped", ModelPath: "/s/e"}
	e.cleanupRequired["stopping"] = proto.InstanceDesiredState{UUID: "stopping", DesiredState: "running", ModelPath: "/s/e"}
	e.cleanupRequired["old"] = proto.InstanceDesiredState{UUID: "old", ModelPath: "/s/d"}
	e.inFlight["awake"], e.inFlight["old"] = true, true
	specs, settled := e.StorageUsers()
	var paths []string
	for _, spec := range specs {
		paths = append(paths, spec.ModelPath)
	}
	sort.Strings(paths)
	// "stopped" has no container left (its teardown was confirmed), so it
	// mounts nothing; "stopping" still has one.
	if !settled || len(paths) != 4 || paths[0] != "/s/a" || paths[1] != "/s/b" || paths[2] != "/s/d" || paths[3] != "/s/e" {
		t.Fatalf("paths=%v settled=%v", paths, settled)
	}
	if running := e.RunningSpecs(); len(running) != 1 {
		t.Fatalf("the awake-only list has %d instances; StorageUsers must not be built on it", len(running))
	}

	// A container being torn down that the agent has no record of.
	e.inFlight["gone"] = true
	if _, settled := e.StorageUsers(); settled {
		t.Fatal("settled while an instance without a record is being torn down")
	}
	delete(e.inFlight, "gone")
	// One removed before the agent ever knew what it mounted.
	e.cleanupRequired["unknown"] = proto.InstanceDesiredState{UUID: "unknown"}
	if _, settled := e.StorageUsers(); settled {
		t.Fatal("settled with an instance to reclaim whose mounts are unknown")
	}
}
