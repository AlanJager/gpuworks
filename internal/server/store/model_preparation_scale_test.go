package store

import (
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestModelPreparationFleetProgress verifies the 1000-node / 3000-copy hot
// path independently of filesystem throughput. Timings are reported, not used
// as machine-dependent unit-test thresholds. The fixed work/query/write bounds
// are assertions; process-level fleet tests measure end-user latency and CPU.
func TestModelPreparationFleetProgress(t *testing.T) {
	f := newStorageFixture(t)
	const nodes, copies = 1000, 3000
	rows := make([]ModelFile, copies)
	for i := range rows {
		rows[i] = ModelFile{ModelID: f.model.ID, NodeID: uint(10000 + i%nodes), StorageID: uint(10000 + i/nodes), State: proto.MFDownloading, Acquisition: proto.ModelAcquireCopy, TransferID: fmt.Sprint(i), SizeBytes: 1 << 30}
	}
	if err := f.st.DB().CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	var writes, queries atomic.Int64
	db := f.st.DB()
	if err := db.Callback().Update().Before("gorm:update").Register("preparation_writes", func(*gorm.DB) { writes.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().Before("gorm:query").Register("preparation_queries", func(*gorm.DB) { queries.Add(1) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Callback().Update().Remove("preparation_writes")
		_ = db.Callback().Query().Remove("preparation_queries")
	})
	durations := make([]time.Duration, 0, copies*3)
	for round := 1; round <= 3; round++ {
		for _, row := range rows {
			start := time.Now()
			err := f.st.UpdateModelFileStatus(row.NodeID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), TransferID: row.TransferID, State: proto.MFDownloading, BytesTotal: row.SizeBytes, BytesDone: int64(round * 4096), Preparation: &proto.ModelPreparation{Attempt: "a", Phase: proto.ModelPhaseCopying, TotalKnown: true}})
			if err != nil {
				t.Fatal(err)
			}
			durations = append(durations, time.Since(start))
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("progress caused %d durable writes", writes.Load())
	}
	if queries.Load() != copies*3 {
		t.Fatalf("got %d queries, want one indexed intent read per frame", queries.Load())
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("1000 nodes / 3000 copies / 9000 frames: p50=%s p99=%s; writes=%d indexed queries=%d", durations[len(durations)/2], durations[len(durations)*99/100], writes.Load(), queries.Load())
}
