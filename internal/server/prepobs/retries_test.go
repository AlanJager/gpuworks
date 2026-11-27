package prepobs

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func retryFrame(key, transfer string, attempt int, wait int64) proto.ModelFileStatus {
	return proto.ModelFileStatus{Key: key, TransferID: transfer, State: proto.MFDownloading, BytesDone: 99, BytesTotal: 100,
		Retry: &proto.ModelFetchRetry{Attempt: attempt, Attempts: 16, WaitSeconds: wait, Cause: "read: connection reset by peer"}}
}

// A retry belongs to the row's current download and lasts its wait plus a
// grace; a frame without one, or of another state, ends it.
func TestRetriesAreBoundToTheRowsCurrentDownload(t *testing.T) {
	r := NewRetries()
	now := time.Unix(1000, 0)
	r.Record(1, retryFrame("7", "t1", 4, 8), now)
	got, ok := r.Observe("7", proto.MFDownloading, "t1", now)
	if !ok || got.Attempt != 4 || got.Attempts != 16 || got.Cause == "" {
		t.Fatalf("retry=%+v ok=%v", got, ok)
	}
	for _, tc := range []struct{ state, transfer string }{{proto.MFDownloading, "t2"}, {proto.MFFailed, "t1"}, {proto.MFPresent, "t1"}} {
		if _, ok := r.Observe("7", tc.state, tc.transfer, now); ok {
			t.Fatalf("retry leaked to row state=%s transfer=%s", tc.state, tc.transfer)
		}
	}
	if _, ok := r.Observe("7", proto.MFDownloading, "t1", now.Add(8*time.Second+RetryGrace-time.Second)); !ok {
		t.Fatal("a retry ended before its wait and grace")
	}
	if _, ok := r.Observe("7", proto.MFDownloading, "t1", now.Add(8*time.Second+RetryGrace)); ok {
		t.Fatal("a retry outlived its wait and grace")
	}
	moving := retryFrame("7", "t1", 0, 0)
	moving.Retry = nil
	r.Record(1, retryFrame("7", "t1", 5, 15), now)
	r.Record(1, moving, now)
	if _, ok := r.Observe("7", proto.MFDownloading, "t1", now); ok || len(r.byNode) != 0 {
		t.Fatal("a frame without a retry did not end it and its index")
	}
}

func TestRetriesForgetNodeOnlyDropsThatNode(t *testing.T) {
	r := NewRetries()
	now := time.Unix(1000, 0)
	r.Record(1, retryFrame("a", "t", 2, 1), now)
	r.Record(2, retryFrame("b", "t", 2, 1), now)
	r.ForgetNode(1)
	if _, ok := r.Observe("a", proto.MFDownloading, "t", now); ok {
		t.Fatal("the disconnected node's retry is still observed")
	}
	if _, ok := r.Observe("b", proto.MFDownloading, "t", now); !ok {
		t.Fatal("another node's retry was dropped")
	}
}

// The map is bounded: a full one drops new retries rather than growing, and
// sweeps expired entries instead of rescanning on every frame.
func TestRetriesAreBoundedAndReclaimExpiredOnes(t *testing.T) {
	r := NewRetries()
	now := time.Unix(1000, 0)
	for i := 0; i < RetryLimit; i++ {
		r.Record(uint(i%1000+1), retryFrame("k"+fmt.Sprint(i), "t", 1, 1), now)
	}
	if len(r.entries) != RetryLimit {
		t.Fatalf("entries=%d", len(r.entries))
	}
	r.Record(1, retryFrame("overflow", "t", 1, 1), now)
	if _, ok := r.Observe("overflow", proto.MFDownloading, "t", now); ok {
		t.Fatal("a full map admitted a new retry")
	}
	later := now.Add(time.Second + RetryGrace + time.Second)
	r.Record(1, retryFrame("later", "t", 1, 1), later)
	if _, ok := r.Observe("later", proto.MFDownloading, "t", later); !ok {
		t.Fatal("expired entries were not reclaimed for a new retry")
	}
	if len(r.entries) > RetryLimit {
		t.Fatalf("entries=%d", len(r.entries))
	}
}

func TestRetriesCountWhatTheLimitKeepsOutAndReportItOncePerInterval(t *testing.T) {
	r := NewRetries()
	now := time.Unix(1000, 0)
	for i := 0; i < RetryLimit; i++ {
		if got := r.Record(1, retryFrame("k"+fmt.Sprint(i), "t", 1, 600), now); got != 0 {
			t.Fatalf("an admitted retry reported %d drops", got)
		}
	}
	if got := r.Record(1, retryFrame("over1", "t", 1, 600), now); got != 1 {
		t.Fatalf("first drop reported %d, want 1", got)
	}
	if got := r.Record(1, retryFrame("over2", "t", 1, 600), now.Add(time.Second)); got != 0 {
		t.Fatalf("a second drop within the interval reported %d, want 0", got)
	}
	if got := r.Dropped(); got != 2 {
		t.Fatalf("Dropped=%d, want 2", got)
	}
	// The entries announced a 600s wait, so nothing expires within a minute:
	// the next drop after the interval reports the running count.
	if got := r.Record(1, retryFrame("over3", "t", 1, 600), now.Add(RetryDropReportEvery)); got != 3 {
		t.Fatalf("drop after the interval reported %d, want 3", got)
	}
	// A frame that ends a retry is not a drop.
	ended := retryFrame("k0", "t", 1, 600)
	ended.Retry = nil
	if got := r.Record(1, ended, now.Add(2*RetryDropReportEvery)); got != 0 || r.Dropped() != 3 {
		t.Fatalf("ending a retry reported %d, Dropped=%d", got, r.Dropped())
	}
}
