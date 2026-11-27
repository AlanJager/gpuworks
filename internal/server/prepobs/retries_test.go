package prepobs

import (
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
