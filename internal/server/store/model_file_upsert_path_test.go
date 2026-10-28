package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// go test ./internal/server/store -run TestUpsertModelFile -count=1
func TestUpsertModelFileRejectsOccupiedSharedPath(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			f := newStorageFixture(t)
			other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: f.model.SourceID})
			if err != nil {
				t.Fatal(err)
			}
			path := SharedModelRelPath(f.model)
			owner, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: path, State: proto.MFPresent, Downloaded: 15})
			if err != nil {
				t.Fatal(err)
			}
			ref := FileRef{ModelID: other.ID, StorageID: f.nfs.ID}
			var before *ModelFile
			if existing {
				before, err = f.st.UpsertModelFile(&ModelFile{ModelID: other.ID, StorageID: f.nfs.ID, Path: "old/path", State: proto.MFFailed, Downloaded: 7, Revision: "keep"})
				if err != nil {
					t.Fatal(err)
				}
			}
			sub := f.st.Bus.Subscribe(8, "modelfile")
			defer sub.Close()
			_, err = f.st.UpsertModelFile(&ModelFile{ModelID: other.ID, StorageID: f.nfs.ID, Path: path, State: proto.MFPresent, Acquisition: proto.ModelAcquireImported, Downloaded: 15})
			var occupied *ModelFilePathOccupiedError
			if !errors.As(err, &occupied) || occupied.ModelID != owner.ModelID || occupied.StorageID != f.nfs.ID || occupied.Path != path {
				t.Fatalf("conflict=%v", err)
			}
			got, err := f.st.ModelFileByID(owner.ID)
			if err != nil || !reflect.DeepEqual(got, owner) {
				t.Fatalf("owner changed: %+v err=%v", got, err)
			}
			got, err = f.st.ModelFileAt(ref)
			if before == nil {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("conflict created row: %+v err=%v", got, err)
				}
			} else if err != nil || !reflect.DeepEqual(got, before) {
				t.Fatalf("old row changed: %+v err=%v", got, err)
			}
			select {
			case event := <-sub.C():
				t.Fatalf("conflict published %+v", event)
			default:
			}
		})
	}
}

func TestUpsertModelFilePreservesPartialUpdateSemantics(t *testing.T) {
	f := newStorageFixture(t)
	row, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: "org/model", State: proto.MFFailed, Revision: "rev", TransferID: "transfer", Downloaded: 7, RateLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: "other/model"})
	if err != nil {
		t.Fatal(err)
	}
	// Empty Path in an update means keep the existing path, even if another row owns "".
	if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: other.ID, StorageID: f.nfs.ID, State: proto.MFPending}); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, State: proto.MFPresent, Acquisition: proto.ModelAcquireImported})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != row.ID || got.Path != row.Path || got.Revision != row.Revision || got.TransferID != row.TransferID || got.Downloaded != row.Downloaded || got.RateLimit != row.RateLimit || got.State != proto.MFPresent || got.Acquisition != proto.ModelAcquireImported {
		t.Fatalf("partial update reset unrelated fields: %+v", got)
	}
	if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, State: proto.MFDeleting}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, State: proto.MFPresent}); !errors.Is(err, ErrCacheDeletionPending) {
		t.Fatalf("deletion guard=%v", err)
	}
	for _, target := range []FileRef{{ModelID: other.ID, StorageID: f.cache.ID}, {ModelID: other.ID, StorageID: f.local.ID, NodeID: f.n1.ID}} {
		if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: target.ModelID, StorageID: target.StorageID, NodeID: target.NodeID, Path: row.Path, State: proto.MFPresent}); err != nil {
			t.Fatalf("independent location: %v", err)
		}
	}
}

func TestUpsertAndPrepareModelFileCompeteForSharedPath(t *testing.T) {
	f := newStorageFixture(t)
	other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: f.model.SourceID})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	path := SharedModelRelPath(f.model)
	go func() {
		<-start
		_, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: path, State: proto.MFPresent, Acquisition: proto.ModelAcquireImported})
		results <- err
	}()
	go func() {
		<-start
		_, err := f.st.PrepareModelDownload(FileRef{ModelID: other.ID, StorageID: f.nfs.ID}, path)
		results <- err
	}()
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		var occupied *ModelFilePathOccupiedError
		if err == nil {
			successes++
		} else if errors.As(err, &occupied) {
			conflicts++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	var rows []ModelFile
	if err := f.st.DB().Where("storage_id = ? AND node_id = 0", f.nfs.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || len(rows) != 1 {
		t.Fatalf("successes=%d conflicts=%d rows=%+v", successes, conflicts, rows)
	}
}
