package store

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestStorageUsersBoundsReplicaReads(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "storage.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	makeStorage := func(name string) *Storage {
		s, err := st.CreateStorage(&Storage{Name: name, Type: proto.StorageNFS, AllNodes: true,
			Path: "/mnt/" + name, Export: "nfs.test:/" + name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := makeStorage("A"), makeStorage("B")
	localID := st.DefaultLocalStorageID()
	models := make([]Model, 2000)
	for i := range models {
		name := "model-" + strconv.Itoa(i)
		models[i] = Model{Name: name, Source: "local", SourceID: name, Revision: "local"}
	}
	if err := st.db.CreateInBatches(&models, 100).Error; err != nil {
		t.Fatal(err)
	}
	var shared []ModelFile
	for i, model := range models {
		shared = append(shared, ModelFile{ModelID: model.ID, StorageID: a.ID, State: proto.MFPresent, Path: model.Name})
		if i < 1000 {
			shared = append(shared, ModelFile{ModelID: model.ID, StorageID: b.ID, State: proto.MFPresent, Path: model.Name})
		}
	}
	if err := st.db.CreateInBatches(&shared, 100).Error; err != nil {
		t.Fatal(err)
	}
	nodes := make([]Node, 1000)
	for i := range nodes {
		nodes[i] = Node{UUID: "node-" + strconv.Itoa(i), Hostname: "node-" + strconv.Itoa(i)}
	}
	if err := st.db.Select("uuid", "hostname").CreateInBatches(&nodes, 100).Error; err != nil {
		t.Fatal(err)
	}
	deps := make([]Deployment, 101)
	for i := range deps {
		deps[i] = Deployment{Name: "service-" + strconv.Itoa(i), ServedName: "service-" + strconv.Itoa(i), ModelID: models[i].ID}
	}
	// This service has no instances and its model exists only on A.
	deps[100].ModelID = models[1999].ID
	if err := st.db.CreateInBatches(&deps, 100).Error; err != nil {
		t.Fatal(err)
	}
	var instances []*Instance
	var reads [][]InstanceModelFile
	var bUsers []uint
	for i, dep := range deps[:100] {
		instances = append(instances, &Instance{UUID: "reader-" + strconv.Itoa(i), DeploymentID: dep.ID, NodeID: nodes[i].ID, State: proto.InstStopped})
		reads = append(reads, []InstanceModelFile{{ModelID: dep.ModelID, ModelFileID: shared[2*i+1].ID, HostPath: "/mnt/B/" + models[i].Name}})
		bUsers = append(bUsers, dep.ID)
	}
	if err := st.CreateInstances(instances, reads); err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 1000} {
		t.Run(strconv.Itoa(count)+"_replicas", func(t *testing.T) {
			start := 0
			if count > 1 {
				start = 1
			}
			var replicas []ModelFile
			for _, node := range nodes[start:count] {
				for _, model := range models[:100] {
					replicas = append(replicas, ModelFile{ModelID: model.ID, StorageID: localID, NodeID: node.ID, Path: "/models/" + model.Name, State: proto.MFPresent})
				}
			}
			if err := st.db.CreateInBatches(&replicas, 100).Error; err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				id    uint
				users []uint
			}{
				{a.ID, []uint{deps[100].ID}}, {b.ID, bUsers}, {localID, nil},
			} {
				trace := &storageReadCounter{Interface: logger.Discard}
				st.readDB = st.readDB.Session(&gorm.Session{Logger: trace})
				users, err := st.StorageUsers(tc.id, []string{proto.MFFailed})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(users.DeploymentIDs, tc.users) {
					t.Fatalf("storage %d users=%v, want %v", tc.id, users.DeploymentIDs, tc.users)
				}
				if len(trace.queries) > 3 || trace.fileRows > int64(len(tc.users)) {
					t.Fatalf("storage %d materialized %d rows in %d queries for %d services", tc.id, trace.fileRows, len(trace.queries), len(tc.users))
				}
				for _, query := range trace.queries {
					var plan []struct{ Detail string }
					if err := st.db.Raw("EXPLAIN QUERY PLAN " + query).Scan(&plan).Error; err != nil {
						t.Fatal(err)
					}
					for _, step := range plan {
						if strings.Contains(step.Detail, "SCAN model_files") {
							t.Fatalf("storage %d scans replicas: %s; query: %s", tc.id, step.Detail, query)
						}
						if strings.Contains(step.Detail, "SEARCH model_files") && !strings.Contains(step.Detail, "INTEGER PRIMARY KEY") && !strings.Contains(step.Detail, "storage_id") {
							t.Fatalf("storage %d reads every replica of a model: %s; query: %s", tc.id, step.Detail, query)
						}
					}
				}
			}
		})
	}
}
