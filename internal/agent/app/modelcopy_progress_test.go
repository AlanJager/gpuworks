package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCopyProgressUsesWritesNotReportsForLiveness(t *testing.T) {
	var frames []proto.ModelFileStatus
	p := newCopyProgress(func(st proto.ModelFileStatus) { frames = append(frames, st) })
	start := time.Now()
	p.begin(start)
	p.mu.Lock()
	p.observation.Phase = proto.ModelPhaseCopying
	p.observation.TotalKnown = true
	p.total = 1000
	p.mu.Unlock()
	p.done.Store(100) // resumed bytes contribute no transfer rate
	p.sample(start.Add(time.Second))
	if frames[1].RateBytesPerS != 0 || frames[1].BytesDone != 100 {
		t.Fatal(frames[1])
	}
	p.done.Add(50)
	p.transferred.Add(50)
	p.sample(start.Add(2 * time.Second))
	if frames[2].RateBytesPerS != 50 {
		t.Fatal(frames[2])
	}
	for _, at := range []time.Duration{copyStallTimeout - time.Nanosecond, copyStallTimeout, copyStallTimeout + time.Second} {
		p.sample(start.Add(2*time.Second + at))
		got := frames[len(frames)-1]
		if got.Preparation.Stalled != (at >= copyStallTimeout) {
			t.Fatalf("at=%v got=%+v", at, got)
		}
	}
	p.waiting.Add(1)
	p.sample(start.Add(10 * copyStallTimeout))
	if frames[len(frames)-1].Preparation.Stalled {
		t.Fatal("intentional pacing reported stalled")
	}
	p.waiting.Add(-1)
	p.sample(start.Add(11 * copyStallTimeout))
	if !frames[len(frames)-1].Preparation.Stalled {
		t.Fatal("heartbeats reset progress")
	}
}

type copyFaultReader struct{ entered, release chan struct{} }

func (r copyFaultReader) Read(b []byte) (int, error) {
	close(r.entered)
	<-r.release
	copy(b, "late")
	return 4, io.EOF
}

type copyShortWriter struct{ bytes.Buffer }

func (w *copyShortWriter) Write(b []byte) (int, error) { return w.Buffer.Write(b[:len(b)/2]) }

func TestCopyBytesCancellationAfterBlockedReadDoesNotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := copyFaultReader{make(chan struct{}), make(chan struct{})}
	var out bytes.Buffer
	var done atomic.Int64
	result := make(chan error, 1)
	go func() { result <- copyModelBytes(ctx, reader, &out, &done, newCopyLimiter(0)) }()
	<-reader.entered
	cancel()
	close(reader.release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if out.Len() != 0 || done.Load() != 0 {
		t.Fatal("canceled read wrote late bytes")
	}
}

func TestCopyBytesShortWriteCountsOnlyAcceptedBytes(t *testing.T) {
	var out copyShortWriter
	var done atomic.Int64
	err := copyModelBytes(context.Background(), bytes.NewBufferString("12345678"), &out, &done, newCopyLimiter(0))
	if !errors.Is(err, io.ErrShortWrite) || done.Load() != 4 || out.String() != "1234" {
		t.Fatalf("%v done=%d output=%q", err, done.Load(), out.String())
	}
}

func TestCopyResumeInvalidatesSameSizeChangedSource(t *testing.T) {
	r := newStorageTestRuntime(t)
	source, target := t.TempDir(), t.TempDir()
	writeModel(t, source, "old-weights")
	files, err := listCopySource(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCopyResume(source, target, files); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "model.safetensors.part"), []byte("old-"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeModel(t, source, "new-weights")
	stamp := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(source, "model.safetensors"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	r.copyModelDir(context.Background(), copyTask("changed", source), target, target)
	if got := r.lastFile(t, "changed"); got.State != proto.MFPresent {
		t.Fatal(got)
	}
	data, err := os.ReadFile(filepath.Join(target, "model.safetensors"))
	if err != nil || string(data) != "new-weights" {
		t.Fatalf("mixed source: %q %v", data, err)
	}
}

func TestBlockedCopyRetainsSlotAndRejectsOverlappingTargets(t *testing.T) {
	r := newStorageTestRuntime(t)
	started, release := make(chan struct{}), make(chan struct{})
	target := filepath.Join(t.TempDir(), "model")
	r.dlMu.Lock()
	r.startTransferLocked(context.Background(), "blocked", target, true, "attempt", func(ctx context.Context) { close(started); <-release })
	<-started
	if !r.copyTaskRunning(proto.ModelFileTask{Key: "blocked", TransferID: "replacement"}, false) {
		t.Fatal("canceled worker lost its slot")
	}
	r.dlMu.Lock()
	available := r.writerSlotAvailableLocked("other", filepath.Join(target, "nested"), true)
	active := r.downloading["blocked"]
	r.dlMu.Unlock()
	if available {
		t.Fatal("overlapping writer admitted")
	}
	close(release)
	<-active.done
	r.dlMu.Lock()
	available = r.writerSlotAvailableLocked("other", target, true)
	r.dlMu.Unlock()
	if !available {
		t.Fatal("returned worker leaked its slot")
	}
}

func TestCopyProgressRequiresStableSamplesForETA(t *testing.T) {
	var latest proto.ModelFileStatus
	p := newCopyProgress(func(st proto.ModelFileStatus) { latest = st })
	start := time.Now()
	p.begin(start)
	p.observation.Phase = proto.ModelPhaseCopying
	for i, delta := range []int64{100, 100, 100, 1000, 0} {
		p.done.Add(delta)
		p.transferred.Add(delta)
		p.sample(start.Add(time.Duration(i+1) * time.Second))
		if latest.Preparation.RateStable != (i == 2) {
			t.Fatalf("sample %d: %+v", i, latest)
		}
	}
}
