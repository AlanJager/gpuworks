package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDeleteNodeCleansImageReplicasOnlyForDeletedNode(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	node, err := s.RegisterNode("delete-images", HashToken("token"), "delete-images")
	if err != nil {
		t.Fatal(err)
	}
	neighbor, err := s.RegisterNode("keep-images", HashToken("neighbor-token"), "keep-images")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{ImageStateAbsent, ImageStatePulling, ImageStatePresent, ImageStateSyncing, ImageStateFailed} {
		img, err := s.UpsertImage(&Image{ImageRef: "example/image:" + state, Engine: "vllm", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, nodeID := range []uint{node.ID, neighbor.ID} {
			if _, err := s.UpsertImageNode(&ImageNode{ImageVersionID: img.ID, NodeID: nodeID, State: state, SizeBytes: 128}); err != nil {
				t.Fatal(err)
			}
		}
	}
	images, err := s.ListImages()
	if err != nil {
		t.Fatal(err)
	}
	neighborRows, err := s.ImageNodesForNode(neighbor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(node.UUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NodeByUUID(node.UUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted node lookup = %v", err)
	}
	if rows, err := s.ImageNodesForNode(node.ID); err != nil || len(rows) != 0 {
		t.Fatalf("deleted node image rows = %v, err = %v", rows, err)
	}
	if rows, err := s.ImageNodesForNode(neighbor.ID); err != nil || !reflect.DeepEqual(rows, neighborRows) {
		t.Fatalf("neighbor replicas changed: %v, err = %v", rows, err)
	}
	if got, err := s.NodeByUUID(neighbor.UUID); err != nil || got.ID != neighbor.ID {
		t.Fatalf("neighbor node changed: %v, err = %v", got, err)
	}
	if got, err := s.ListImages(); err != nil || !reflect.DeepEqual(got, images) {
		t.Fatalf("image catalog changed: %v, err = %v", got, err)
	}
}

func TestDeleteNodeImageCleanupRollsBack(t *testing.T) {
	for _, table := range []string{"image_nodes", "nodes"} {
		t.Run(table, func(t *testing.T) {
			s := newTestStore(t)
			t.Cleanup(func() { _ = s.Close() })
			node, err := s.RegisterNode("rollback-images", HashToken("token"), "rollback-images")
			if err != nil {
				t.Fatal(err)
			}
			img, err := s.UpsertImage(&Image{ImageRef: "example/image:rollback", Engine: "vllm", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.UpsertImageNode(&ImageNode{ImageVersionID: img.ID, NodeID: node.ID, State: ImageStatePresent})
			if err != nil {
				t.Fatal(err)
			}
			model, err := s.UpsertModel(&Model{Source: "local", SourceID: "rollback-model", Name: "rollback-model"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent}); err != nil {
				t.Fatal(err)
			}
			files, err := s.ModelFilesForNode(node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DB().Exec("CREATE TRIGGER reject_delete BEFORE DELETE ON " + table + " BEGIN SELECT RAISE(ABORT, 'injected cleanup failure'); END").Error; err != nil {
				t.Fatal(err)
			}
			sub := s.Bus.Subscribe(4, "node")
			defer sub.Close()
			if err := s.DeleteNode(node.UUID); err == nil || !strings.Contains(err.Error(), "injected cleanup failure") {
				t.Fatalf("delete error = %v", err)
			}
			if _, err := s.NodeByUUID(node.UUID); err != nil {
				t.Fatalf("node lost after rollback: %v", err)
			}
			if got, err := s.ImageNode(img.ID, node.ID); err != nil || !reflect.DeepEqual(got, before) {
				t.Fatalf("image replica changed after rollback: %v, err = %v", got, err)
			}
			if got, err := s.ModelFilesForNode(node.ID); err != nil || !reflect.DeepEqual(got, files) {
				t.Fatalf("model files changed after rollback: %v, err = %v", got, err)
			}
			select {
			case event := <-sub.C():
				t.Fatalf("failed deletion published event: %+v", event)
			default:
			}
		})
	}
}
