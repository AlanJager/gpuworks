package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Each Prepare* puts a row into a fresh state. They share the columns every
// one of them resets; the ones only some reset stay as they were for the
// others: a public download clears the resolved revision, a peer sync clears
// the rate limit, and a copy and a registration clear neither.
func TestPrepareModelFileResetsExactlyItsColumns(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	ref := FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
	// A row a previous transfer left in a used state.
	used := func() *ModelFile {
		t.Helper()
		row, err := st.UpsertModelFile(&ModelFile{ModelID: ref.ModelID, StorageID: ref.StorageID, NodeID: ref.NodeID, Path: "/old/path",
			State: proto.MFFailed, Acquisition: proto.ModelAcquirePeer, Revision: "abc123", RateLimit: 5, LastRateLimit: 7,
			TransferID: "t-1", TransferSourceNodeID: f.n2.ID, SizeBytes: 100, Downloaded: 40, TransferredBytes: 30, RateBPS: 9, Message: "boom"})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	type want struct {
		state, acquisition, revision string
		rateLimit                    int
		size, downloaded             int64
	}
	for _, tc := range []struct {
		name string
		do   func() (*ModelFile, error)
		want want
	}{
		{"download", func() (*ModelFile, error) { return st.PrepareModelDownload(ref, "Qwen/new") },
			want{proto.MFPending, proto.ModelAcquireDownload, "", 5, 100, 0}},
		{"peer sync", func() (*ModelFile, error) { return st.PrepareModelPeerSync(ref, "Qwen/new") },
			want{proto.MFPending, proto.ModelAcquireAuto, "abc123", 0, 100, 0}},
		{"copy", func() (*ModelFile, error) { return st.PrepareModelCopy(ref, "Qwen/new") },
			want{proto.MFPending, proto.ModelAcquireCopy, "abc123", 0, 100, 0}},
		{"register", func() (*ModelFile, error) { return st.RegisterModelFile(ref, "Qwen/new", 55) },
			want{proto.MFRegistered, proto.ModelAcquireRegistered, "abc123", 5, 55, 55}},
	} {
		used()
		got, err := tc.do()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Path != "Qwen/new" || got.State != tc.want.state || got.Acquisition != tc.want.acquisition ||
			got.Revision != tc.want.revision || got.RateLimit != tc.want.rateLimit || got.LastRateLimit != 7 ||
			got.SizeBytes != tc.want.size || got.Downloaded != tc.want.downloaded {
			t.Errorf("%s: row = %+v, want %+v", tc.name, got, tc.want)
		}
		if (tc.name == "copy" && (got.TransferID == "" || got.TransferID == "t-1")) || (tc.name != "copy" && got.TransferID != "") || got.TransferSourceNodeID != 0 || got.TransferredBytes != 0 || got.RateBPS != 0 || got.Message != "" {
			t.Errorf("%s: the previous transfer's leftovers survived: %+v", tc.name, got)
		}
	}
}
