package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

type storageReadCounter struct {
	logger.Interface
	fileRows int64
}

func (l *storageReadCounter) Trace(ctx context.Context, begin time.Time, query func() (string, int64), err error) {
	statement, rows := query()
	if strings.Contains(statement, "model_files") {
		l.fileRows += rows
	}
}

func TestStorageReferencesUseReaderAndDistinctProjection(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "storage.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	target, err := st.StorageByID(st.DefaultLocalStorageID())
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateStorage(&Storage{Name: "other", Type: proto.StorageLocal, Path: t.TempDir(), AllNodes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&Model{Name: "model", Source: "local", SourceID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []Node
	for _, uuid := range []string{"first", "second", "third"} {
		node, err := st.RegisterNode(uuid, HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, *node)
	}
	var targetFile *ModelFile
	for i, node := range nodes {
		file, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, StorageID: target.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/model"})
		if err != nil {
			t.Fatal(err)
		}
		targetFile = file
		state := proto.MFPresent
		if i == 2 {
			state = proto.MFFailed
		}
		if _, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, StorageID: other.ID, NodeID: node.ID, State: state, Path: "/other/model"}); err != nil {
			t.Fatal(err)
		}
	}
	dep, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "reader", ServedName: "reader", ModelID: model.ID},
		nodelabel.Requirements{nodelabel.KeyStorage: {strconv.FormatUint(uint64(target.ID), 10)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstances([]*Instance{{UUID: "reader", DeploymentID: dep.ID, NodeID: targetFile.NodeID, State: proto.InstStopped}},
		[][]InstanceModelFile{{{ModelID: model.ID, ModelFileID: targetFile.ID, HostPath: targetFile.Path}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Callback().Query().Before("gorm:query").Register(t.Name(), func(tx *gorm.DB) {
		tx.AddError(errors.New("reference query reached the writer pool"))
	}); err != nil {
		t.Fatal(err)
	}
	reads := &storageReadCounter{Interface: logger.Discard}
	st.readDB = st.readDB.Session(&gorm.Session{Logger: reads})
	refs, err := st.StorageModelReferences(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(refs.ModelIDs, []uint{model.ID}) || len(refs.ModelUsers) != 1 || refs.ModelUsers[0].ID != dep.ID || refs.ModelUsers[0].ModelID != model.ID {
		t.Fatalf("model/reference projection: %+v", refs)
	}
	if !slices.Equal(refs.DirectUsers.DeploymentIDs, []uint{dep.ID}) || !slices.Equal(refs.DirectUsers.InstanceUUIDs, []string{"reader"}) {
		t.Fatalf("explicit readers and pinned requirements: %+v", refs.DirectUsers)
	}
	var states []string
	for _, file := range refs.OtherFiles {
		if file.ModelID != model.ID || file.ID != 0 || file.NodeID != 0 || file.Path != "" {
			t.Fatalf("expected only model/state projection: %+v", file)
		}
		states = append(states, file.State)
	}
	slices.Sort(states)
	if !slices.Equal(states, []string{proto.MFFailed, proto.MFPresent}) {
		t.Fatalf("source states must be distinct and unfiltered: %v", states)
	}
	// One model ID, two source states, one direct reader; not six file rows.
	if reads.fileRows != 4 {
		t.Fatalf("reference queries materialized %d file rows, want 4 projections", reads.fileRows)
	}
}
