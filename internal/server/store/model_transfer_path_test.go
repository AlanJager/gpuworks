package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestLocalModelTransfersKeepDifferentRevisionsSeparateAndStable(t *testing.T) {
	f := newStorageFixture(t)
	other, err := f.st.UpsertModel(&Model{Source: f.model.Source, SourceID: f.model.SourceID, Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == f.model.ID {
		t.Fatal("fixture must have different revision identities")
	}
	path := ModelArtifactPath(f.model)
	firstRef := FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
	first, err := f.st.PrepareModelDownload(firstRef, path)
	if err != nil || first.Path != path {
		t.Fatalf("first transfer=%+v err=%v", first, err)
	}
	// Real completion reports replace the pending relative path with the
	// agent's absolute path. That owner must still reserve the directory.
	firstAbsolute := filepath.Join("/var/lib/gpuworks/models", path)
	if err := f.st.UpdateModelFileStatus(f.n1.ID, proto.ModelFileStatus{
		Key: ModelFileKey(first.ID), State: proto.MFPresent, Path: firstAbsolute,
		TransferID: first.TransferID, BytesTotal: 15, BytesDone: 15,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := f.st.ModelFileByID(first.ID)
	if err != nil || completed.Path != firstAbsolute {
		t.Fatalf("completed first owner=%+v err=%v", completed, err)
	}
	ref := FileRef{ModelID: other.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
	var chosen string
	for _, prepare := range []func(FileRef, string) (*ModelFile, error){f.st.PrepareModelDownload, f.st.PrepareModelCopy, f.st.PrepareModelPeerSync} {
		row, err := prepare(ref, path)
		if err != nil || row.Path == path || (chosen != "" && row.Path != chosen) {
			t.Fatalf("different revision shares or moves transfer directory: row=%+v chosen=%q err=%v", row, chosen, err)
		}
		chosen = row.Path
	}
	second, err := f.st.ModelFileAt(ref)
	if err != nil {
		t.Fatal(err)
	}
	secondAbsolute := filepath.Join("/var/lib/gpuworks/models", chosen)
	if err := f.st.UpdateModelFileStatus(f.n1.ID, proto.ModelFileStatus{
		Key: ModelFileKey(second.ID), State: proto.MFPresent, Path: secondAbsolute,
		TransferID: second.TransferID, BytesTotal: 15, BytesDone: 15,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err = f.st.ModelFileByID(second.ID)
	if err != nil || completed.Path != secondAbsolute {
		t.Fatalf("completed fallback owner=%+v err=%v", completed, err)
	}
	// Removing the old owner must not make a retry move existing files again,
	// including after the allocated fallback became an absolute reported path.
	if err := f.st.DB().Delete(&ModelFile{}, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, prepare := range []func(FileRef, string) (*ModelFile, error){f.st.PrepareModelDownload, f.st.PrepareModelCopy, f.st.PrepareModelPeerSync} {
		row, err := prepare(ref, path)
		if err != nil || row.Path != chosen {
			t.Fatalf("retry after original owner deletion: %+v chosen=%q err=%v", row, chosen, err)
		}
	}
	// The same relative directory is safe on another node or another storage.
	for _, target := range []FileRef{
		{ModelID: other.ID, StorageID: f.local.ID, NodeID: f.n2.ID},
		{ModelID: other.ID, StorageID: f.cache.ID, NodeID: f.n1.ID},
	} {
		copy, err := f.st.PrepareModelDownload(target, path)
		if err != nil || copy.Path != path {
			t.Fatalf("unrelated location moved: %+v err=%v", copy, err)
		}
	}
}

func TestLocalModelTransferDoesNotSubstituteAnAlreadyOccupiedFallback(t *testing.T) {
	f := newStorageFixture(t)
	other, err := f.st.UpsertModel(&Model{Source: f.model.Source, SourceID: f.model.SourceID, Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := f.st.UpsertModel(&Model{Source: "local", SourceID: "manually-registered"})
	if err != nil {
		t.Fatal(err)
	}
	path := ModelArtifactPath(f.model)
	fallback := fmt.Sprintf("%s-model-%d", path, other.ID)
	for _, occupied := range []ModelFile{
		{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: path, State: proto.MFPresent},
		{ModelID: third.ID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: fallback, State: proto.MFRegistered},
	} {
		if _, err := f.st.UpsertModelFile(&occupied); err != nil {
			t.Fatal(err)
		}
	}
	ref := FileRef{ModelID: other.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
	_, err = f.st.PrepareModelDownload(ref, path)
	var occupied *ModelFilePathOccupiedError
	if !errors.As(err, &occupied) || occupied.ModelID != third.ID || occupied.Path != fallback {
		t.Fatalf("occupied fallback=%v", err)
	}
	if row, err := f.st.ModelFileAt(ref); err != ErrNotFound {
		t.Fatalf("failed allocation created row=%+v err=%v", row, err)
	}
}

func TestLocalModelRegistrationKeepsTheExplicitDirectory(t *testing.T) {
	f := newStorageFixture(t)
	other, err := f.st.UpsertModel(&Model{Source: "local", SourceID: "alias"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint{f.model.ID, other.ID} {
		row, err := f.st.RegisterModelFile(FileRef{ModelID: id, StorageID: f.local.ID, NodeID: f.n1.ID}, "/operator/chosen", 13)
		if err != nil || row.Path != "/operator/chosen" {
			t.Fatalf("explicit registration=%+v err=%v", row, err)
		}
	}
}

// The source snapshot can say absent while an API request has already started
// a transfer. An automatic ensure must observe that owner without resetting it.
func TestEnsureModelAcquisitionPreservesAnExistingTransfer(t *testing.T) {
	for _, peer := range []bool{false, true} {
		t.Run(fmt.Sprintf("peer=%t", peer), func(t *testing.T) {
			f := newStorageFixture(t)
			ref := FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
			row, err := f.st.PrepareModelDownload(ref, "operator/chosen")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.SetDownloadDeployment(row.ID, row.TransferID, 71); err != nil {
				t.Fatal(err)
			}
			if peer {
				if err := f.st.BeginModelSync(ref, f.n2.ID, "operator-peer-transfer", 15, 128); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.st.ModelFileByID(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				got, err := f.st.EnsureModelAcquisition(ref, ModelArtifactPath(f.model), 99)
				if err != nil || !reflect.DeepEqual(got, before) {
					t.Fatalf("ensure changed existing transfer: before=%+v after=%+v err=%v", before, got, err)
				}
			}
			after, err := f.st.ModelFileByID(row.ID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("persisted transfer changed: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestEnsureModelAcquisitionDoesNotReviveADeletingFile(t *testing.T) {
	f := newStorageFixture(t)
	ref := FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
	row, err := f.st.UpsertModelFile(&ModelFile{ModelID: ref.ModelID, StorageID: ref.StorageID, NodeID: ref.NodeID,
		Path: "awaiting-cleanup", State: proto.MFDeleting, TransferID: "delete-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.st.EnsureModelAcquisition(ref, ModelArtifactPath(f.model), 99); !errors.Is(err, ErrCacheDeletionPending) || got != nil {
		t.Fatalf("ensure deleting file=%+v err=%v", got, err)
	}
	persisted, err := f.st.ModelFileByID(row.ID)
	if err != nil || persisted.State != proto.MFDeleting || persisted.TransferID != "delete-owner" || persisted.Path != row.Path {
		t.Fatalf("deleting row changed=%+v err=%v", persisted, err)
	}
}
