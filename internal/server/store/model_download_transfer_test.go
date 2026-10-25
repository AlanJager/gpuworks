package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func downloadTransferFixture(t *testing.T) (*Store, *Model, *Node, FileRef) {
	t.Helper()
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "test/download", Revision: "main", Name: "download"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("download-node", HashToken("token"), "download-node")
	if err != nil {
		t.Fatal(err)
	}
	return st, model, node, FileRef{ModelID: model.ID, NodeID: node.ID}
}

// Every public-download preparation is a new transfer: the row gets a new
// TransferID and the task sent to the agent carries it.
func TestPrepareModelDownloadNamesEachPreparationInItsTask(t *testing.T) {
	st, model, node, ref := downloadTransferFixture(t)
	first, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	if first.TransferID == "" || second.TransferID == "" || first.TransferID == second.TransferID {
		t.Fatalf("preparations did not each name a transfer: %q then %q", first.TransferID, second.TransferID)
	}
	task, ok := st.ModelFileTask(second, model)
	if !ok || task.Acquisition != proto.ModelAcquireDownload || task.TransferID != second.TransferID {
		t.Fatalf("task = %+v, ok=%v; want the download named %q", task, ok, second.TransferID)
	}
	ds, err := st.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.ModelFiles) != 1 || ds.ModelFiles[0].TransferID != second.TransferID {
		t.Fatalf("desired state replays %+v, want transfer %q", ds.ModelFiles, second.TransferID)
	}
}

// A download the user re-prepared (a retry, another revision) replaces the one
// that was running. The earlier download's late reports name the old transfer
// and must not decide the new row.
func TestLateReportOfReplacedDownloadIsDropped(t *testing.T) {
	st, model, node, ref := downloadTransferFixture(t)
	first, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	key := ModelFileKey(second.ID)
	for _, state := range []string{proto.MFDownloading, proto.MFPresent, proto.MFFailed} {
		if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
			Key: key, State: state, TransferID: first.TransferID, Acquisition: proto.ModelAcquireDownload,
			BytesDone: 99, BytesTotal: 100, Message: "from the replaced download",
		}); err != nil {
			t.Fatal(err)
		}
		row, err := st.ModelFileByID(second.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != proto.MFPending || row.Downloaded != 0 || row.Message != "" || row.TransferID != second.TransferID {
			t.Fatalf("a %s report of the replaced download changed the row: %+v", state, row)
		}
	}

	for _, state := range []string{proto.MFDownloading, proto.MFPresent} {
		if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
			Key: key, State: state, TransferID: second.TransferID, Acquisition: proto.ModelAcquireDownload,
			BytesDone: 100, BytesTotal: 100, Path: "/models/Qwen/a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	row, err := st.ModelFileByID(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != proto.MFPresent || row.TransferID != second.TransferID || row.Downloaded != 100 {
		t.Fatalf("the current download's reports were not applied: %+v", row)
	}
	if task, ok := st.ModelFileTask(row, model); !ok || task.TransferID != second.TransferID {
		t.Fatalf("task after completion = %+v, ok=%v", task, ok)
	}
}

// An agent that predates download IDs reports none. Its reports are still
// accepted and the row keeps the transfer the server named.
func TestDownloadReportsWithoutTransferIDAreAccepted(t *testing.T) {
	st, _, node, ref := downloadTransferFixture(t)
	row, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	key := ModelFileKey(row.ID)
	for _, state := range []string{proto.MFDownloading, proto.MFPresent} {
		if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
			Key: key, State: state, Acquisition: proto.ModelAcquireDownload, BytesDone: 100, BytesTotal: 100, Path: "/models/Qwen/a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ModelFileByID(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.MFPresent || got.Downloaded != 100 || got.TransferID != row.TransferID {
		t.Fatalf("legacy agent reports = %+v, want present under %q", got, row.TransferID)
	}

	// A failure reported without an ID by a legacy agent lands as well.
	failed, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: key, State: proto.MFFailed, Acquisition: proto.ModelAcquireDownload, Message: "disk error",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ModelFileByID(failed.ID)
	if got.State != proto.MFFailed || got.Message != "disk error" {
		t.Fatalf("legacy failure = %+v", got)
	}
}

// A server that predates download IDs leaves them empty; a new agent then
// echoes an empty ID and the row is decided as before.
func TestEmptyDownloadTransferIDRowStillTakesEmptyReports(t *testing.T) {
	st, _, node, ref := downloadTransferFixture(t)
	row, err := st.UpsertModelFile(&ModelFile{ModelID: ref.ModelID, NodeID: ref.NodeID, Path: "Qwen/a", State: proto.MFPending, Acquisition: proto.ModelAcquireDownload})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: ModelFileKey(row.ID), State: proto.MFPresent, Acquisition: proto.ModelAcquireDownload, BytesDone: 5, BytesTotal: 5, Path: "/models/Qwen/a",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ModelFileByID(row.ID)
	if got.State != proto.MFPresent || got.TransferID != "" {
		t.Fatalf("row = %+v", got)
	}
}
