package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func newBatchRow(id string, user uint, created time.Time) *Batch {
	return &Batch{
		ID: id, UserID: user, Endpoint: "/v1/chat/completions", InputFileID: "file-" + id,
		CompletionWindow: "24h", Status: BatchValidating, Total: 10,
		CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour),
	}
}

func TestBatchFilesCreateListAndDelete(t *testing.T) {
	st := newTestStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		owner := uint(1 + i%2)
		purpose := BatchFilePurposeInput
		if i == 4 {
			purpose = BatchFilePurposeOutput
		}
		f := &BatchFile{ID: fmt.Sprintf("file-%d", i), UserID: owner, Purpose: purpose, Filename: "in.jsonl", Bytes: 10, Lines: 2, CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := st.CreateBatchFile(f); err != nil {
			t.Fatal(err)
		}
	}
	one := uint(1)
	mine, err := st.ListBatchFiles(BatchOwnerFilter{UserID: &one}, "", "", 10)
	if err != nil || len(mine) != 3 || mine[0].ID != "file-4" || mine[2].ID != "file-0" {
		t.Fatalf("user 1 files = %+v err=%v", mine, err)
	}
	all, _ := st.ListBatchFiles(BatchOwnerFilter{}, "", "", 10)
	if len(all) != 5 {
		t.Fatalf("all files = %d", len(all))
	}
	onlyInput, _ := st.ListBatchFiles(BatchOwnerFilter{}, BatchFilePurposeInput, "", 10)
	if len(onlyInput) != 4 {
		t.Fatalf("input files = %d", len(onlyInput))
	}
	page, _ := st.ListBatchFiles(BatchOwnerFilter{}, "", "file-3", 2)
	if len(page) != 2 || page[0].ID != "file-2" || page[1].ID != "file-1" {
		t.Fatalf("page after file-3 = %+v", page)
	}
	if _, err := st.GetBatchFile("file-9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file = %v", err)
	}

	// A file an unfinished batch reads stays; once the batch ends it goes.
	if err := st.CreateBatch(&Batch{ID: "batch_a", InputFileID: "file-0", Endpoint: "/v1/embeddings", CompletionWindow: "24h", Status: BatchInProgress, ExpiresAt: base.Add(24 * time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBatchFile("file-0"); !errors.Is(err, ErrBatchFileInUse) {
		t.Fatalf("delete in use = %v", err)
	}
	if err := st.FinishBatch("batch_a", BatchFinish{Status: BatchCompleted, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBatchFile("file-0"); err != nil {
		t.Fatalf("delete after the batch finished: %v", err)
	}
	if err := st.DeleteBatchFile("file-0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice = %v", err)
	}
}

func TestBatchCreateListMetadataAndCursor(t *testing.T) {
	st := newTestStore(t)
	sub := st.Bus.Subscribe(8, "batch")
	defer sub.Close()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		b := newBatchRow(fmt.Sprintf("batch_%d", i), uint(1+i%2), base.Add(time.Duration(i)*time.Minute))
		var meta map[string]string
		if i == 3 {
			meta = map[string]string{"team": "nlp", "run": "7"}
		}
		if err := st.CreateBatch(b, meta); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case ev := <-sub.C():
		if ev.Topic != "batch" || ev.Reason != BatchValidating {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event after the commit")
	}
	two := uint(2)
	mine, total, err := st.ListBatchesPage(BatchOwnerFilter{UserID: &two}, nil, 0, 10)
	if err != nil || total != 2 || len(mine) != 2 || mine[0].ID != "batch_3" {
		t.Fatalf("user 2 batches = %+v total=%d err=%v", mine, total, err)
	}
	pg, total, _ := st.ListBatchesPage(BatchOwnerFilter{}, nil, 1, 2)
	if total != 5 || len(pg) != 2 || pg[0].ID != "batch_3" || pg[1].ID != "batch_2" {
		t.Fatalf("page = %+v total=%d", pg, total)
	}
	none, total, _ := st.ListBatchesPage(BatchOwnerFilter{}, []string{BatchCompleted, BatchFailed}, 0, 10)
	if total != 0 || len(none) != 0 {
		t.Fatalf("completed = %+v", none)
	}
	after, err := st.ListBatchesAfter(BatchOwnerFilter{}, "batch_3", 10)
	if err != nil || len(after) != 3 || after[0].ID != "batch_2" {
		t.Fatalf("after batch_3 = %+v err=%v", after, err)
	}
	meta, err := st.BatchMetadataOf([]string{"batch_3", "batch_4"})
	if err != nil || len(meta) != 1 || meta["batch_3"]["team"] != "nlp" || meta["batch_3"]["run"] != "7" {
		t.Fatalf("metadata = %+v err=%v", meta, err)
	}
	if _, err := st.GetBatch("batch_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing batch = %v", err)
	}
}

// A run's lifecycle: started once, checkpointed without events, cancel is
// idempotent, finishing writes the files and counts in one step, and nothing
// moves a finished batch.
func TestBatchLifecycleTransitions(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	if err := st.CreateBatch(newBatchRow("batch_x", 1, now), nil); err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(16, "batch")
	defer sub.Close()

	if ok, err := st.StartBatch("batch_x", now); err != nil || !ok {
		t.Fatalf("start = %v %v", ok, err)
	}
	if ok, _ := st.StartBatch("batch_x", now); ok {
		t.Fatal("a second start took effect")
	}
	if err := st.SaveBatchProgress("batch_x", BatchProgress{Completed: 4, Failed: 1, Offset: 321}); err != nil {
		t.Fatal(err)
	}
	b, _ := st.GetBatch("batch_x")
	if b.Status != BatchInProgress || b.InProgressAt == nil || b.Completed != 4 || b.Failed != 1 || b.CheckpointOffset != 321 {
		t.Fatalf("after a checkpoint: %+v", b)
	}
	if err := st.BeginBatchFinalize("batch_x", now); err != nil {
		t.Fatal(err)
	}
	b, _ = st.GetBatch("batch_x")
	if b.Status != BatchFinalizing || b.FinalizingAt == nil {
		t.Fatalf("finalizing: %+v", b)
	}
	out := &BatchFile{ID: "file-out", UserID: 1, Purpose: BatchFilePurposeOutput, Filename: "batch_x_output.jsonl", Bytes: 5}
	errf := &BatchFile{ID: "file-err", UserID: 1, Purpose: BatchFilePurposeOutput, Filename: "batch_x_error.jsonl", Bytes: 5}
	if err := st.FinishBatch("batch_x", BatchFinish{Status: BatchCompleted, At: now, Completed: 9, Failed: 1, Output: out, Error: errf}); err != nil {
		t.Fatal(err)
	}
	b, _ = st.GetBatch("batch_x")
	if b.Status != BatchCompleted || b.CompletedAt == nil || b.OutputFileID != "file-out" || b.ErrorFileID != "file-err" || b.Completed != 9 || b.Failed != 1 {
		t.Fatalf("finished: %+v", b)
	}
	if _, err := st.GetBatchFile("file-out"); err != nil {
		t.Fatalf("output row: %v", err)
	}
	if err := st.FinishBatch("batch_x", BatchFinish{Status: BatchCancelled, At: now}); !errors.Is(err, ErrBatchTerminal) {
		t.Fatalf("finish twice = %v", err)
	}
	if err := st.SaveBatchProgress("batch_x", BatchProgress{Completed: 99}); err != nil {
		t.Fatal(err)
	}
	if b, _ = st.GetBatch("batch_x"); b.Completed != 9 {
		t.Fatalf("a checkpoint moved a finished batch: %+v", b)
	}
	if _, err := st.RequestBatchCancel("batch_x", now); !errors.Is(err, ErrBatchTerminal) {
		t.Fatalf("cancel finished = %v", err)
	}
	if err := st.FinishBatch("batch_none", BatchFinish{Status: BatchCompleted}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finish missing = %v", err)
	}
	if err := st.FinishBatch("batch_x", BatchFinish{Status: BatchInProgress}); err == nil {
		t.Fatal("a non-terminal status finished a batch")
	}

	// Cancel of a running batch: cancelling once, the same row again.
	st.CreateBatch(newBatchRow("batch_y", 1, now), nil)
	st.StartBatch("batch_y", now)
	got, err := st.RequestBatchCancel("batch_y", now)
	if err != nil || got.Status != BatchCancelling || got.CancellingAt == nil {
		t.Fatalf("cancel = %+v %v", got, err)
	}
	again, err := st.RequestBatchCancel("batch_y", now.Add(time.Minute))
	if err != nil || again.Status != BatchCancelling || !again.CancellingAt.Equal(*got.CancellingAt) {
		t.Fatalf("second cancel = %+v %v", again, err)
	}
	if err := st.BeginBatchFinalize("batch_y", now); err != nil {
		t.Fatal(err)
	}
	if b, _ = st.GetBatch("batch_y"); b.Status != BatchCancelling {
		t.Fatalf("a cancelling batch moved to %s", b.Status)
	}

	pending, err := st.UnfinishedBatches()
	if err != nil || len(pending) != 1 || pending[0].ID != "batch_y" {
		t.Fatalf("unfinished = %+v err=%v", pending, err)
	}
	// Progress checkpoints announce nothing: x's start, finalizing and
	// completion plus y's start and cancel (the creates were before the
	// subscription).
	var reasons []string
	for len(sub.C()) > 0 {
		reasons = append(reasons, (<-sub.C()).Reason)
	}
	want := []string{"in_progress", "finalizing", "completed", "validating", "in_progress", "cancelling"}
	if fmt.Sprint(reasons) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", reasons, want)
	}
}

func TestDeleteBatchOutputsBeforeKeepsInputs(t *testing.T) {
	st := newTestStore(t)
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for _, f := range []*BatchFile{
		{ID: "file-in", Purpose: BatchFilePurposeInput, Filename: "a", CreatedAt: old},
		{ID: "file-old", Purpose: BatchFilePurposeOutput, Filename: "b", CreatedAt: old},
		{ID: "file-new", Purpose: BatchFilePurposeOutput, Filename: "c", CreatedAt: time.Now().UTC()},
	} {
		if err := st.CreateBatchFile(f); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := st.DeleteBatchOutputsBefore(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil || len(ids) != 1 || ids[0] != "file-old" {
		t.Fatalf("expired = %v err=%v", ids, err)
	}
	for _, id := range []string{"file-in", "file-new"} {
		if _, err := st.GetBatchFile(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}
