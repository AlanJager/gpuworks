package store

import (
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestModelPreparationProgressIsTransientAndIntentBound(t *testing.T) {
	f := newStorageFixture(t)
	ref := FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
	file, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	report := proto.ModelFileStatus{Key: ModelFileKey(file.ID), TransferID: file.TransferID, Acquisition: proto.ModelAcquireCopy, State: proto.MFDownloading, BytesTotal: 10000, Preparation: &proto.ModelPreparation{Attempt: "snapshot", Phase: proto.ModelPhaseCopying, TotalKnown: true}}
	if err := f.st.UpdateModelFileStatus(f.n1.ID, report); err != nil {
		t.Fatal(err)
	}
	persisted, _ := f.st.ModelFileByID(file.ID)
	updated := persisted.UpdatedAt
	for i := 1; i <= 1000; i++ {
		report.BytesDone = int64(i)
		report.TransferBytesDone = int64(i)
		if err := f.st.UpdateModelFileStatus(f.n1.ID, report); err != nil {
			t.Fatal(err)
		}
	}
	persisted, _ = f.st.ModelFileByID(file.ID)
	if persisted.Downloaded != 0 || !persisted.UpdatedAt.Equal(updated) {
		t.Fatalf("progress wrote durable row: %+v", persisted)
	}
	if err := f.st.UpdateModelFileStatus(f.n2.ID, report); err == nil {
		t.Fatal("foreign node accepted")
	}
	replacement, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	report.State = proto.MFPresent
	report.Preparation = nil
	if err := f.st.UpdateModelFileStatus(f.n1.ID, report); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.ModelFileByID(file.ID)
	if got.State != proto.MFPending || got.TransferID != replacement.TransferID {
		t.Fatal("old success replaced new intent")
	}
	report.TransferID = replacement.TransferID
	report.BytesDone = 10000
	if err := f.st.UpdateModelFileStatus(f.n1.ID, report); err != nil {
		t.Fatal(err)
	}
	got, _ = f.st.ModelFileByID(file.ID)
	if got.State != proto.MFPresent || got.Downloaded != 10000 {
		t.Fatal("terminal result not durable")
	}
}

func TestModelSourceSnapshotMatchesSingleSource(t *testing.T) {
	f := newStorageFixture(t)
	snapshot, err := f.st.ModelSourcesFor([]uint{f.model.ID}, []*Node{f.n1, f.n2})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*Node{f.n1, f.n2} {
		for _, rules := range []SourceRules{{}, {SaveTo: f.local.ID}, {Pinned: []uint{9999}}} {
			single, err := f.st.ModelSourceOn(f.model.ID, node, rules)
			if err != nil {
				t.Fatal(err)
			}
			batch := snapshot.Source(f.model.ID, node.ID, rules)
			if batch.Action != single.Action || batch.Reason != single.Reason || batch.Location.Storage.ID != single.Location.Storage.ID || batch.Location.HostPath != single.Location.HostPath {
				t.Fatalf("single=%+v batch=%+v", single, batch)
			}
		}
	}
}

func TestModelFileLegacyNullTransferIDAcceptsStatus(t *testing.T) {
	f := newStorageFixture(t)
	file, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, NodeID: f.n1.ID, State: proto.MFPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().Model(&ModelFile{}).Where("id = ?", file.ID).Update("transfer_id", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateModelFileStatus(f.n1.ID, proto.ModelFileStatus{Key: ModelFileKey(file.ID), State: proto.MFPresent, BytesTotal: 10, BytesDone: 10}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.ModelFileByID(file.ID)
	if got.State != proto.MFPresent || got.Downloaded != 10 {
		t.Fatalf("legacy null transfer ID lost report: %+v", got)
	}
}

func TestModelSourceSnapshotPreservesNodeOverridesAndLabelScope(t *testing.T) {
	f := newStorageFixture(t)
	f.n1.Labels = `{"rack":"a"}`
	for i, targets := range [][]StorageNode{
		{{NodeID: f.n1.ID, Path: "/first"}, {NodeID: f.n2.ID, Path: "/second"}, {LabelKey: "rack", LabelValue: "a"}},
		{{LabelKey: "rack", LabelValue: "a"}},
	} {
		storage, err := f.st.CreateStorage(&Storage{Name: fmt.Sprintf("scoped-%d", i), Type: proto.StorageLocal, Enabled: true, Path: "/default"}, targets)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.st.ModelSourcesFor([]uint{f.model.ID}, []*Node{f.n1, f.n2})
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range []*Node{f.n1, f.n2} {
			rules := SourceRules{Pinned: []uint{storage.ID}}
			single, err := f.st.ModelSourceOn(f.model.ID, node, rules)
			if err != nil {
				t.Fatal(err)
			}
			batch := snapshot.Source(f.model.ID, node.ID, rules)
			if single.Action != batch.Action || single.Reason != batch.Reason || single.Location.HostPath != batch.Location.HostPath || single.Location.Storage.ID != batch.Location.Storage.ID {
				t.Fatalf("node %s: single=%+v batch=%+v", node.UUID, single, batch)
			}
		}
	}
}

func TestModelSourceSnapshotUsesScopeIndexes(t *testing.T) {
	f := newStorageFixture(t)
	type query struct {
		sql  string
		args []any
	}
	var queries []query
	if err := f.st.DB().Callback().Query().After("gorm:query").Register("scope_index_queries", func(db *gorm.DB) {
		if db.Statement.Table == "storage_nodes" || db.Statement.Table == "storage_node_states" {
			queries = append(queries, query{db.Statement.SQL.String(), append([]any(nil), db.Statement.Vars...)})
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer f.st.DB().Callback().Query().Remove("scope_index_queries")
	if _, err := f.st.ModelSourcesFor([]uint{f.model.ID}, []*Node{f.n1}); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("scope queries=%d", len(queries))
	}
	for _, q := range queries {
		var plan []struct{ Detail string }
		if err := f.st.DB().Raw("EXPLAIN QUERY PLAN "+q.sql, q.args...).Scan(&plan).Error; err != nil {
			t.Fatal(err)
		}
		indexed := false
		for _, row := range plan {
			if strings.Contains(row.Detail, "SCAN ") || strings.Contains(row.Detail, "TEMP B-TREE") {
				t.Fatalf("unscoped scan/sort: %s: %v", q.sql, plan)
			}
			indexed = indexed || strings.Contains(row.Detail, "idx_storage_node")
		}
		if !indexed {
			t.Fatalf("scope index unused: %s: %v", q.sql, plan)
		}
	}
}

// A copy's rate limit is the copy's own: the next copy into the same place
// starts unlimited unless its caller sets one, while a retry keeps it.
func TestModelCopyRateLimitBelongsToOneCopy(t *testing.T) {
	f := newStorageFixture(t)
	ref := FileRef{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID}
	file, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetModelCopyRate(ref, 2048); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetModelFileState(file.ID, proto.MFFailed, "read failed"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RetryModelCopy(file.ID, file.TransferID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.st.ModelFileByID(file.ID); got.RateLimit != 2048 {
		t.Fatalf("retry dropped the copy's limit: %d", got.RateLimit)
	}
	again, err := f.st.PrepareModelCopy(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	if again.RateLimit != 0 {
		t.Fatalf("a new copy inherited the previous copy's limit: %d", again.RateLimit)
	}
	if got, _ := f.st.ModelFileByID(file.ID); got.RateLimit != 0 {
		t.Fatalf("stored limit after a new copy = %d", got.RateLimit)
	}
}
