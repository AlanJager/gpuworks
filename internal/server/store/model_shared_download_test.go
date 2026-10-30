package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestSharedDownloadClaimDoesNotReplaceNewlyCreatedLocation(t *testing.T) {
	f := newStorageFixture(t)
	ref := FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}
	// Both requests saw no location before choosing their writers. Once the
	// first owns it, the stale absence in the other request grants no right to
	// reset its transfer or change its writer.
	winner, err := f.st.PrepareSharedModelDownload(ref, "Qwen/model", f.n1.ID, 17, nil)
	if err != nil {
		t.Fatal(err)
	}
	if winner.State != proto.MFPending || winner.TransferID == "" || winner.WriterNodeID != f.n1.ID || winner.DownloadDeploymentID != 17 {
		t.Fatalf("incomplete claim: %+v", winner)
	}
	if _, err := f.st.PrepareSharedModelDownload(ref, "Other/path", f.n2.ID, 18, nil); !errors.Is(err, ErrModelDownloadInactive) {
		t.Fatalf("stale absent observer replaced winner: %v", err)
	}
	got, err := f.st.ModelFileAt(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != winner.ID || got.TransferID != winner.TransferID || got.Path != winner.Path || got.WriterNodeID != winner.WriterNodeID || got.DownloadDeploymentID != winner.DownloadDeploymentID {
		t.Fatalf("loser modified winning task: %+v", got)
	}
}
