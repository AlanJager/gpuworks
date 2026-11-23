package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Both requests observe the canceled row before either stop acknowledgement
// returns. Only one may claim the next shared transfer and its writer.
func TestSharedDownloadConcurrentRetryClaimsWriterOnce(t *testing.T) {
	r := newSaveToRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	other, err := r.st.RegisterNode("other-writer", store.HashToken("other"), "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.ApplyStorageStatus(other.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: r.nfs.ID, State: proto.StorageStateReady, Writable: true, Path: r.nfs.Path}}}); err != nil {
		t.Fatal(err)
	}
	ref := store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}
	row, err := r.st.PrepareModelDownload(ref, store.SharedModelRelPath(r.model))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetModelFileWriter(row.ID, r.node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CancelModelDownload(row.ID, row.TransferID, "canceled"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	r.h.d.CallNode = func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		if method != proto.RPCModelDownloadStop {
			return nil, fmt.Errorf("unexpected RPC %s", method)
		}
		entered <- struct{}{}
		select {
		case <-release:
			return json.RawMessage(`{}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// Each request answers whether it started the download or was refused
	// as a conflict.
	results := make(chan string, 2)
	for _, uuid := range []string{r.node.UUID, other.UUID} {
		go func(uuid string) {
			writer, err := r.h.StartSharedDownloadOn(r.model, r.nfs, uuid)
			switch {
			case err == nil && writer != nil:
				results <- "started"
			case KindOf(err) == KindConflict:
				results <- "conflict"
			default:
				results <- fmt.Sprintf("writer=%v err=%v", writer, err)
			}
		}(uuid)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not observe same canceled transfer")
		}
	}
	close(release)
	first, second := <-results, <-results
	if !((first == "started" && second == "conflict") || (second == "started" && first == "conflict")) {
		t.Fatalf("competing shared writers accepted: %s, %s", first, second)
	}
}

// A retry that names no writer goes back to the writer the failed download
// had, not to whichever ready node sorts first; once that writer cannot write
// to the storage any more, another one is picked.
func TestSharedDownloadRetryKeepsThePreviousWriterWhileItCanWrite(t *testing.T) {
	r := newSaveToRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	chosen, err := r.st.RegisterNode("chosen-writer", store.HashToken("chosen"), "chosen")
	if err != nil {
		t.Fatal(err)
	}
	report := func(state string, writable bool) {
		t.Helper()
		if err := r.st.ApplyStorageStatus(chosen.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: r.nfs.ID, State: state, Writable: writable, Path: r.nfs.Path}}}); err != nil {
			t.Fatal(err)
		}
	}
	report(proto.StorageStateReady, true)
	if chosen.ID < r.node.ID {
		t.Fatalf("the chosen writer (id %d) must not be the one a fresh pick prefers (id %d)", chosen.ID, r.node.ID)
	}
	fail := func() {
		t.Helper()
		row, err := r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.st.UpdateModelFileStatus(row.WriterNodeID, proto.ModelFileStatus{
			Key: store.ModelFileKey(row.ID), State: proto.MFFailed, TransferID: row.TransferID,
			Acquisition: proto.ModelAcquireDownload, Message: "list files: context deadline exceeded",
		}); err != nil {
			t.Fatal(err)
		}
		if row, err = r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}); err != nil || row.State != proto.MFFailed {
			t.Fatalf("row = %+v, err = %v; want a failed download", row, err)
		}
	}
	if writer, err := r.h.StartSharedDownloadOn(r.model, r.nfs, chosen.UUID); err != nil || writer == nil || writer.ID != chosen.ID {
		t.Fatalf("first download: writer = %+v, err = %v", writer, err)
	}
	fail()
	if writer, err := r.h.StartSharedDownloadOn(r.model, r.nfs, ""); err != nil || writer == nil || writer.ID != chosen.ID {
		t.Fatalf("retry without a writer: writer = %+v, err = %v; want the previous writer %d", writer, err, chosen.ID)
	}
	fail()
	report(proto.StorageStateReady, false) // read-only there now
	if writer, err := r.h.StartSharedDownloadOn(r.model, r.nfs, ""); err != nil || writer == nil || writer.ID != r.node.ID {
		t.Fatalf("retry after the previous writer lost write access: writer = %+v, err = %v; want %d", writer, err, r.node.ID)
	}
}
