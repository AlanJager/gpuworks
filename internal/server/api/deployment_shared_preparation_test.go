package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentPreparationFollowsSharedStorageDownload(t *testing.T) {
	for _, kind := range []string{proto.StorageNFS, proto.StorageSharedDir, proto.StorageNFSCache} {
		t.Run(kind, func(t *testing.T) {
			st := modelEvictionStore(t, "shared-preparation-"+kind)
			model, node := modelEvictionFixture(t, st)
			if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
				t.Fatal(err)
			}
			storage, err := st.CreateStorage(&store.Storage{Name: "shared", Type: kind, Enabled: true, AllNodes: true, Path: "/shared", Export: "fixture:/models", CachePath: "/cache"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storage.ID, State: proto.StorageStateReady, Path: "/shared", CachePath: "/cache", Writable: true}}}); err != nil {
				t.Fatal(err)
			}
			file := &store.ModelFile{ModelID: model.ID, StorageID: storage.ID, WriterNodeID: node.ID, Path: "model", State: proto.MFDownloading, Acquisition: proto.ModelAcquireDownload, SizeBytes: 0, Downloaded: 40, RateBPS: 10, Message: "weights 2/5"}
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if kind == proto.StorageNFSCache {
				if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, NodeID: node.ID, Path: "/cache/old", State: proto.MFFailed, Acquisition: proto.ModelAcquireCopy, TransferID: "stale"}); err != nil {
					t.Fatal(err)
				}
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: "shared-download", ModelID: model.ID, NodeID: node.ID, SaveToStorageID: storage.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetDeploymentRequirements(dep.ID, map[string][]string{"gpuworks.ai/storage": {fmt.Sprint(storage.ID)}}, store.PlacementSpread); err != nil {
				t.Fatal(err)
			}
			prep := newTestPreparation()
			handler := &apiHandlers{d: Deps{Store: st, Preparation: prep}}
			assertProgress := func(state, phase string, known bool) dto.ModelNodePresence {
				t.Helper()
				detail := handler.deploymentDTO(dep, false)
				list, err := newListLookup(st, prep, []store.Deployment{*dep}, nil)
				if err != nil {
					t.Fatal(err)
				}
				shown, recorded := deploymentModelProgress(list, dep)
				if len(shown) != 1 || len(recorded) != 1 || !reflect.DeepEqual(shown, detail.ModelProgress) {
					t.Fatalf("list=%+v recorded=%+v detail=%+v", shown, recorded, detail.ModelProgress)
				}
				item := shown[0]
				if item.State != state || item.Downloaded != file.Downloaded || item.SizeBytes != file.SizeBytes || item.Message != file.Message || item.Preparation.Phase != phase || item.Preparation.TotalKnown != known {
					t.Fatalf("wrong shared progress: %+v view=%+v", item, item.Preparation)
				}
				for _, response := range []dto.Deployment{detail, handler.deploymentDTOFrom(list, dep, false)} {
					wire, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					var decoded map[string]any
					if err := json.Unmarshal(wire, &decoded); err != nil {
						t.Fatal(err)
					}
					progress := decoded["model_progress"].([]any)[0].(map[string]any)
					for key, want := range map[string]any{"node_uuid": node.UUID, "storage_id": float64(storage.ID), "state": state, "downloaded": float64(file.Downloaded), "size_bytes": float64(file.SizeBytes), "message": file.Message} {
						if progress[key] != want {
							t.Errorf("wire %s=%#v, want %#v", key, progress[key], want)
						}
					}
				}
				return item
			}
			assertProgress(proto.MFDownloading, "downloading", false)
			file.SizeBytes = 100
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if item := assertProgress(proto.MFDownloading, "downloading", true); item.RateBps != 10 || item.Progress != 0.4 || item.Preparation.Reason != "" {
				t.Fatalf("download=%+v view=%+v", item, item.Preparation)
			}
			file.State, file.Message = proto.MFFailed, "download interrupted"
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if item := assertProgress(proto.MFFailed, "waiting", true); item.RateBps != 0 || item.Preparation.Reason != "MODEL_PREPARATION_FAILED" || item.Preparation.Retryable {
				t.Fatalf("failed=%+v view=%+v", item, item.Preparation)
			}
			file.State, file.SizeBytes, file.Downloaded = proto.MFPresent, 100, 100
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			if kind == proto.StorageNFSCache {
				// The shared download is complete: the node now reports its own
				// cache copy, without reusing a shared transfer observation.
				cacheFile, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, NodeID: node.ID, Path: "/cache/model", State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy, TransferID: "new-cache", SizeBytes: 100})
				if err != nil {
					t.Fatal(err)
				}
				prep.record(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(cacheFile.ID), State: proto.MFDownloading, TransferID: "stale", BytesDone: 99, BytesTotal: 100, Preparation: &proto.ModelPreparation{Phase: proto.ModelPhaseCopying, TotalKnown: true}})
				progress := handler.deploymentDTO(dep, false).ModelProgress
				if len(progress) != 1 || progress[0].State != proto.MFDownloading || progress[0].Downloaded != 0 || progress[0].Preparation.TotalKnown {
					t.Fatalf("cache copy borrowed upstream progress: %+v", progress)
				}
				prep.record(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(cacheFile.ID), State: proto.MFDownloading, TransferID: "new-cache", BytesDone: 20, BytesTotal: 100, Preparation: &proto.ModelPreparation{Phase: proto.ModelPhaseCopying, TotalKnown: true}})
				progress = handler.deploymentDTO(dep, false).ModelProgress
				if len(progress) != 1 || progress[0].Downloaded != 20 || progress[0].Preparation.Phase != proto.ModelPhaseCopying {
					t.Fatalf("cache copy lost its own progress: %+v", progress)
				}
			} else {
				assertProgress(proto.MFPresent, "ready", false)
			}
		})
	}
}
