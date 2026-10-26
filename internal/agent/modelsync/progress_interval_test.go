package modelsync

import (
	"bytes"
	"testing"
	"time"
)

func TestSmallSlowTransferReportsBeforeEightMiB(t *testing.T) {
	var reports []int64
	r := newMeasuredReader(bytes.NewReader([]byte("slow")), 0, func(n int64) { reports = append(reports, n) })
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if len(reports) == 0 || reports[len(reports)-1] != 2 {
		t.Fatalf("slow moving transfer has no periodic byte progress: %v", reports)
	}
}
