package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestLocalModelTransferRetryRejectsOccupiedAllocatedDirectory(t *testing.T) {
	for _, operation := range []string{"download", "peer", "copy"} {
		for _, requested := range []string{"allocated", "preferred"} {
			t.Run(operation+"/"+requested, func(t *testing.T) {
				f := newStorageFixture(t)
				other, err := f.st.UpsertModel(&Model{Source: f.model.Source, SourceID: f.model.SourceID, Revision: "main"})
				if err != nil {
					t.Fatal(err)
				}
				third, err := f.st.UpsertModel(&Model{Source: "local", SourceID: "manual-owner"})
				if err != nil {
					t.Fatal(err)
				}
				preferred := ModelArtifactPath(f.model)
				original, err := f.st.PrepareModelDownload(FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID}, preferred)
				if err != nil {
					t.Fatal(err)
				}
				ref := FileRef{ModelID: other.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
				allocated, err := f.st.PrepareModelDownload(ref, preferred)
				if err != nil {
					t.Fatal(err)
				}
				if allocated.Path == preferred || filepath.IsAbs(allocated.Path) {
					t.Fatalf("expected a distinct relative allocation, got %+v", allocated)
				}
				if err := f.st.BeginModelSync(ref, f.n2.ID, "retained-transfer", 91, 13); err != nil {
					t.Fatal(err)
				}
				if err := f.st.UpdateModelFileStatus(f.n1.ID, proto.ModelFileStatus{
					Key: ModelFileKey(allocated.ID), TransferID: "retained-transfer", State: proto.MFFailed,
					BytesDone: 9, BytesTotal: 91, Message: "retain this failed attempt",
				}); err != nil {
					t.Fatal(err)
				}
				before, err := f.st.ModelFileAt(ref)
				if err != nil {
					t.Fatal(err)
				}
				owner, err := f.st.RegisterModelFile(FileRef{ModelID: third.ID, StorageID: f.local.ID, NodeID: f.n1.ID}, filepath.Join(f.local.Path, allocated.Path), 41)
				if err != nil {
					t.Fatal(err)
				}
				sub := f.st.Bus.Subscribe(8, "modelfile")
				defer sub.Close()
				path := allocated.Path
				if requested == "preferred" {
					path = preferred
				}
				prepare := map[string]func(FileRef, string) (*ModelFile, error){
					"download": f.st.PrepareModelDownload,
					"peer":     f.st.PrepareModelPeerSync,
					"copy":     f.st.PrepareModelCopy,
				}[operation]
				row, err := prepare(ref, path)
				var occupied *ModelFilePathOccupiedError
				if !errors.As(err, &occupied) || occupied.StorageID != ref.StorageID || occupied.ModelID != third.ID || occupied.Path != allocated.Path || row != nil {
					t.Fatalf("retry substituted an occupied allocated directory: row=%+v err=%v", row, err)
				}
				for _, want := range []*ModelFile{before, original, owner} {
					got, err := f.st.ModelFileByID(want.ID)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("failed retry changed row: got=%+v want=%+v err=%v", got, want, err)
					}
				}
				select {
				case event := <-sub.C():
					t.Fatalf("failed retry published %+v", event)
				default:
				}
			})
		}
	}
}
