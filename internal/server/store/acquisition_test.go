package store

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestResolveImageRefPrecedence(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	first, err := s.UpsertImage(&Image{ImageRef: "org/runner:v1", RegistrySources: `["quay.io/org/runner:v1"]`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertImage(&Image{ImageRef: "other/runner:v1", RegistrySources: `["quay.io/org/runner:v1"]`}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveImageRef("quay.io/org/runner:v1")
	if err != nil || got.ID != first.ID {
		t.Fatalf("alias = %+v, %v", got, err)
	}
	exact, err := s.UpsertImage(&Image{ImageRef: "quay.io/org/runner:v1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.ResolveImageRef("quay.io/org/runner:v1")
	if err != nil || got.ID != exact.ID {
		t.Fatalf("exact = %+v, %v", got, err)
	}
	if _, err := s.ResolveImageRef("missing:v1"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestFindModelArtifactAnyRevisionPrefersReadyNode(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	node, err := s.RegisterNode("local", HashToken("token"), "local")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "Org/Model", Revision: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "Org/Model", Revision: "second"})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{proto.MFDownloading, proto.MFPresent, proto.MFRegistered} {
		if _, err := s.UpsertModelFile(&ModelFile{ModelID: second.ID, NodeID: node.ID, State: state, Path: "/models/pinned"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.FindModelArtifactAnyRevision("org/model", " HuggingFace ", "", node.ID)
		want := second.ID
		if state == proto.MFDownloading {
			want = first.ID
		}
		if err != nil || got.ID != want {
			t.Fatalf("state=%s got=%+v err=%v want=%d", state, got, err, want)
		}
	}
	got, err := s.FindModelArtifactAnyRevision("Org/Model", "huggingface", "", 0)
	if err != nil || got.ID != first.ID {
		t.Fatalf("no node = %+v, %v", got, err)
	}
	if _, err := s.FindModelArtifact("Org/Model", "huggingface", "", ""); err != ErrNotFound {
		t.Fatalf("exact matching became wildcard: %v", err)
	}
	if _, err := s.FindModelArtifactAnyRevision("Org/Model", "huggingface", "subdir", node.ID); err != ErrNotFound {
		t.Fatalf("subfolder: %v", err)
	}
}
