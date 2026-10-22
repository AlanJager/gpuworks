package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// taskPathFields is what a task says about where its model lives, as the
// agent receives it.
type taskPathFields struct {
	Path       string `json:"path"`
	RelPath    string `json:"rel_path"`
	TargetPath string `json:"target_path"`
}

func wirePathFields(t *testing.T, task proto.ModelFileTask) taskPathFields {
	t.Helper()
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var got taskPathFields
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("task wire %s: %v", raw, err)
	}
	return got
}

// A task names its target in Path (an absolute host directory) or in RelPath
// (below the storage root), never both, and still fills TargetPath for agents
// that predate the two.
func TestModelFileTaskNamesTheKindOfItsPath(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})

	// A local storage's row holds the node's own directory once it is written.
	local, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: "/data/models/x", State: proto.MFPresent, SizeBytes: 15})
	if err != nil {
		t.Fatal(err)
	}
	// A node cache row is the node's own directory too.
	cache, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, NodeID: f.n1.ID, Path: "/var/lib/gpuworks/cache/archive-cache/m", State: proto.MFPresent, SizeBytes: 15})
	if err != nil {
		t.Fatal(err)
	}
	// A shared storage's row is relative to the storage root.
	shared, err := st.PrepareModelDownload(FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}, SharedModelRelPath(f.model))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelFileWriter(shared.ID, f.n1.ID); err != nil {
		t.Fatal(err)
	}
	// A node's row that has not been written yet is relative as well.
	pending, err := st.PrepareModelDownload(FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n2.ID}, "Qwen/Qwen3-8B")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		row  *ModelFile
		want taskPathFields
	}{
		{"local row", local, taskPathFields{Path: "/data/models/x", TargetPath: "/data/models/x"}},
		{"node cache row", cache, taskPathFields{Path: "/var/lib/gpuworks/cache/archive-cache/m", TargetPath: "/var/lib/gpuworks/cache/archive-cache/m"}},
		{"shared row", shared, taskPathFields{RelPath: "Qwen/Qwen3-8B", TargetPath: "Qwen/Qwen3-8B"}},
		{"row not written yet", pending, taskPathFields{RelPath: "Qwen/Qwen3-8B", TargetPath: "Qwen/Qwen3-8B"}},
	} {
		row, err := st.ModelFileByID(tc.row.ID)
		if err != nil {
			t.Fatal(err)
		}
		task, ok := st.ModelFileTask(row, f.model)
		if !ok {
			t.Fatalf("%s: no task", tc.name)
		}
		if got := wirePathFields(t, task); got != tc.want {
			t.Errorf("%s: path fields = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
