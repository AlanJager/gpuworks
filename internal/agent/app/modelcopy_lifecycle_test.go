package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCopyValidationBarrierAndSourceChange(t *testing.T) {
	r := newStorageTestRuntime(t)
	source, target := t.TempDir(), t.TempDir()
	writeModel(t, source, "old-weights")
	changed := false
	attempts := map[string]bool{}
	r.Bind(func(kind string, value any) error {
		if kind != proto.TStatusModelFile {
			return nil
		}
		status := value.(proto.ModelFileStatus)
		if p := status.Preparation; p != nil {
			attempts[p.Attempt] = true
			if p.Phase == proto.ModelPhaseVerifying {
				if _, err := os.Stat(filepath.Join(target, modelCompleteMarker)); !os.IsNotExist(err) {
					t.Error("completion marker exists before verification")
				}
				if !changed {
					writeModel(t, source, "new-weights")
					stamp := time.Now().Add(time.Hour)
					if err := os.Chtimes(filepath.Join(source, "model.safetensors"), stamp, stamp); err != nil {
						t.Fatal(err)
					}
					changed = true
				}
			}
		}
		return nil
	})
	r.copyModelDir(context.Background(), copyTask("mutating", source), target, target)
	if len(attempts) != 2 {
		t.Fatalf("attempt IDs=%v", attempts)
	}
	contents, err := os.ReadFile(filepath.Join(target, "model.safetensors"))
	if err != nil || string(contents) != "new-weights" {
		t.Fatalf("mixed contents=%q err=%v", contents, err)
	}
	if _, ok := validCompletionMarker(target, copyTask("mutating", source)); !ok {
		t.Fatal("missing validated completion")
	}
}

func TestCopyCanceledAtVerificationCannotInstall(t *testing.T) {
	r := newStorageTestRuntime(t)
	source, work, final := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "final")
	writeModel(t, source, "weights")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := false
	r.Bind(func(kind string, value any) error {
		if kind != proto.TStatusModelFile {
			return nil
		}
		status := value.(proto.ModelFileStatus)
		if status.Preparation != nil && status.Preparation.Phase == proto.ModelPhaseVerifying {
			cancel()
		}
		ready = ready || status.State == proto.MFPresent
		return nil
	})
	r.copyModelDir(ctx, copyTask("cancel", source), work, final)
	if ready {
		t.Fatal("canceled verification reported ready")
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("installed canceled model: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, modelCompleteMarker)); !os.IsNotExist(err) {
		t.Fatalf("canceled copy has marker: %v", err)
	}
}

func TestCopyReportsBeforeLargeFileCompletes(t *testing.T) {
	r := newStorageTestRuntime(t)
	source, target := t.TempDir(), t.TempDir()
	writeModel(t, source, strings.Repeat("w", 3<<20))
	task := copyTask("large", source)
	task.RateLimitKBs = 1024
	observed := make(chan struct{}, 1)
	r.Bind(func(kind string, value any) error {
		if kind != proto.TStatusModelFile {
			return nil
		}
		st := value.(proto.ModelFileStatus)
		if st.Preparation != nil && st.Preparation.Phase == proto.ModelPhaseCopying && st.BytesDone > 1<<20 && st.BytesDone < st.BytesTotal {
			if _, err := os.Stat(filepath.Join(target, "model.safetensors")); os.IsNotExist(err) {
				select {
				case observed <- struct{}{}:
				default:
				}
			}
		}
		return nil
	})
	r.copyModelDir(context.Background(), task, target, target)
	select {
	case <-observed:
	default:
		t.Fatal("no partial progress while the large file was still open")
	}
}

func TestCopyStopWaitsForWorkerAndFencesOldIntent(t *testing.T) {
	r := newStorageTestRuntime(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.dlMu.Lock()
	r.startTransferLocked(context.Background(), "copy", t.TempDir(), true, "old", func(ctx context.Context) { close(entered); <-release })
	<-entered
	raw, _ := json.Marshal(proto.ModelCopyStopParams{Key: "copy", TransferID: "old"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.stopModelCopy(ctx, raw); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r.dlMu.Lock()
	active := r.downloading["copy"]
	r.dlMu.Unlock()
	if active == nil {
		t.Fatal("timeout released writer")
	}
	close(release)
	<-active.done
	if _, err := r.stopModelCopy(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if !r.copyTaskRunning(proto.ModelFileTask{Key: "copy", TransferID: "old"}, false) {
		t.Fatal("old intent can restart")
	}
	if r.copyTaskRunning(proto.ModelFileTask{Key: "copy", TransferID: "new"}, false) {
		t.Fatal("new intent is fenced")
	}
}

func TestCopyWithdrawalRetainsSlotUntilWorkerReturns(t *testing.T) {
	n := newStorageTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := &modelDownload{copy: true, transferID: "old", target: "/copy", cancel: cancel, done: make(chan struct{})}
	n.downloading["copy"] = active
	n.reconcileModelFile(context.Background(), proto.ModelFileTask{Key: "copy", DesiredState: "absent"})
	if ctx.Err() != context.Canceled || n.downloading["copy"] != active {
		t.Fatal("withdrawal did not cancel or released a live writer")
	}
	n.stoppedCopies = map[string]string{"removed": "old", "kept": "kept"}
	n.pruneWaiting([]proto.ModelFileTask{{Key: "kept", TransferID: "kept"}})
	if len(n.stoppedCopies) != 1 || n.stoppedCopies["kept"] != "kept" {
		t.Fatal("full-state pruning left stale fences")
	}
}

func TestCopyCanceledBeforeInstallDoesNotRename(t *testing.T) {
	work, final := t.TempDir(), filepath.Join(t.TempDir(), "final")
	writeModel(t, work, "weights")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := installStagedModelContext(ctx, work, final, proto.ModelFileTask{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatal("canceled installation created destination")
	}
	if _, err := os.Stat(filepath.Join(work, "model.safetensors")); err != nil {
		t.Fatal("canceled installation removed resumable source")
	}
}
