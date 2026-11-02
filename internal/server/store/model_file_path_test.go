package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// go test ./internal/server/store -run 'TestPrepareModelFile.*(Path|Atomic)' -count=1
func TestPrepareModelFileRejectsOccupiedSharedPath(t *testing.T) {
	for _, state := range []string{proto.MFPresent, proto.MFFailed, proto.MFDeleting} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%t", state, existing), func(t *testing.T) {
				f := newStorageFixture(t)
				other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: f.model.SourceID, Name: "other"})
				if err != nil {
					t.Fatal(err)
				}
				path := SharedModelRelPath(f.model)
				owner, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: path, State: state, SizeBytes: 15})
				if err != nil {
					t.Fatal(err)
				}
				ref := FileRef{ModelID: other.ID, StorageID: f.nfs.ID}
				var before *ModelFile
				if existing {
					before, err = f.st.UpsertModelFile(&ModelFile{ModelID: other.ID, StorageID: f.nfs.ID, Path: "old/path", State: proto.MFFailed, Message: "keep", Downloaded: 7})
					if err != nil {
						t.Fatal(err)
					}
				}
				sub := f.st.Bus.Subscribe(8, "modelfile")
				defer sub.Close()
				for _, prepare := range []func(FileRef, string) (*ModelFile, error){f.st.PrepareModelDownload, f.st.PrepareModelCopy, f.st.PrepareModelPeerSync} {
					_, err := prepare(ref, path)
					want := fmt.Sprintf("shared storage %d path %q is occupied by model %d", f.nfs.ID, path, f.model.ID)
					if err == nil || err.Error() != want {
						t.Fatalf("error=%v, want %s", err, want)
					}
					var occupied *ModelFilePathOccupiedError
					if !errors.As(err, &occupied) || occupied.StorageID != f.nfs.ID || occupied.Path != path || occupied.ModelID != f.model.ID {
						t.Fatalf("missing conflict identity: %v", err)
					}
					got, err := f.st.ModelFileByID(owner.ID)
					if err != nil || !reflect.DeepEqual(got, owner) {
						t.Fatalf("owner changed: %+v, err=%v", got, err)
					}
					got, err = f.st.ModelFileAt(ref)
					if before == nil {
						if err != ErrNotFound {
							t.Fatalf("failed preparation left row=%+v, err=%v", got, err)
						}
					} else if err != nil || !reflect.DeepEqual(got, before) {
						t.Fatalf("requester's old row changed: %+v, err=%v", got, err)
					}
					select {
					case event := <-sub.C():
						t.Fatalf("failed preparation published %+v", event)
					default:
					}
				}
			})
		}
	}
}

func TestPrepareModelFilePathScopeAndRetry(t *testing.T) {
	f := newStorageFixture(t)
	path := SharedModelRelPath(f.model)
	ref := FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}
	first, err := f.st.PrepareModelDownload(ref, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.st.PrepareModelDownload(ref, path)
	if err != nil || second.ID != first.ID || second.TransferID == first.TransferID {
		t.Fatalf("retry=%+v, err=%v", second, err)
	}
	other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: "other/model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []FileRef{
		{ModelID: other.ID, StorageID: f.cache.ID},
		{ModelID: other.ID, StorageID: f.local.ID, NodeID: f.n1.ID},
		{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n2.ID},
		{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID},
	} {
		if _, err := f.st.PrepareModelDownload(target, path); err != nil {
			t.Fatalf("target=%+v: %v", target, err)
		}
	}
	if _, err := f.st.PrepareModelDownload(FileRef{ModelID: other.ID, StorageID: f.nfs.ID}, "other/path"); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareModelFileConcurrentSharedPath(t *testing.T) {
	f := newStorageFixture(t)
	other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: f.model.SourceID})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []uint{f.model.ID, other.ID} {
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			<-start
			_, err := f.st.PrepareModelDownload(FileRef{ModelID: id, StorageID: f.nfs.ID}, SharedModelRelPath(f.model))
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "is occupied by model") {
			conflicts++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	var rows []ModelFile
	if err := f.st.DB().Where("storage_id = ? AND node_id = 0", f.nfs.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || len(rows) != 1 || rows[0].State != proto.MFPending {
		t.Fatalf("successes=%d conflicts=%d rows=%+v", successes, conflicts, rows)
	}
}

func TestPrepareModelFileAtomicOnWriteFailure(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			f := newStorageFixture(t)
			ref := FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}
			var before *ModelFile
			if existing {
				var err error
				before, err = f.st.UpsertModelFile(&ModelFile{ModelID: ref.ModelID, StorageID: ref.StorageID, Path: "old/path", State: proto.MFFailed, Downloaded: 7, Message: "keep"})
				if err != nil {
					t.Fatal(err)
				}
			}
			sub := f.st.Bus.Subscribe(8, "modelfile")
			defer sub.Close()
			if err := f.st.DB().Exec(`CREATE TRIGGER reject_model_prepare BEFORE UPDATE ON model_files BEGIN SELECT RAISE(ABORT, 'write rejected'); END`).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.PrepareModelDownload(ref, SharedModelRelPath(f.model)); err == nil || !strings.Contains(err.Error(), "write rejected") {
				t.Fatalf("error=%v", err)
			}
			row, err := f.st.ModelFileAt(ref)
			if before == nil {
				if err != ErrNotFound {
					t.Fatalf("failed preparation left row=%+v, err=%v", row, err)
				}
			} else if err != nil || !reflect.DeepEqual(row, before) {
				t.Fatalf("failed preparation changed row=%+v, err=%v", row, err)
			}
			select {
			case event := <-sub.C():
				t.Fatalf("failed preparation published %+v", event)
			default:
			}
		})
	}
}

func TestModelFilePathWriteErrorClassifiesOnlyOccupiedSharedPaths(t *testing.T) {
	f := newStorageFixture(t)
	path := SharedModelRelPath(f.model)
	if _, err := f.st.PrepareModelDownload(FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}, path); err != nil {
		t.Fatal(err)
	}
	other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: f.model.SourceID})
	if err != nil {
		t.Fatal(err)
	}
	ref := FileRef{ModelID: other.ID, StorageID: f.nfs.ID}
	duplicate := f.st.DB().Create(&ModelFile{ModelID: ref.ModelID, StorageID: ref.StorageID, Path: path}).Error
	if duplicate == nil {
		t.Fatal("fixture did not hit the real unique index")
	}
	var occupied *ModelFilePathOccupiedError
	if err := modelFilePathWriteError(f.st.DB(), ref, path, duplicate); !errors.As(err, &occupied) || occupied.ModelID != f.model.ID {
		t.Fatalf("unique conflict not translated: %v", err)
	}
	modelDuplicate := f.st.DB().Create(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: "other/path"}).Error
	if modelDuplicate == nil {
		t.Fatal("fixture did not hit the model identity index")
	}
	for _, tc := range []struct {
		ref  FileRef
		path string
		err  error
	}{
		{ref, path, errors.New("disk failure")},
		{FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}, path, modelDuplicate},
		{ref, "unoccupied/path", duplicate},
		{FileRef{ModelID: ref.ModelID, StorageID: ref.StorageID, NodeID: f.n1.ID}, path, duplicate},
	} {
		if got := modelFilePathWriteError(f.st.DB(), tc.ref, tc.path, tc.err); got != tc.err {
			t.Fatalf("unrelated write error changed: %v -> %v", tc.err, got)
		}
	}
}
