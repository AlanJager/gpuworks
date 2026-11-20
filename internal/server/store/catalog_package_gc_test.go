package store

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestCatalogPackageGCRequiresCommittedBarrier(t *testing.T) {
	st := catalogReferenceFixture(t)
	for _, want := range []int64{1, 2} {
		got, err := st.CatalogPackageReferencesForGC(t.Context())
		if err != nil || !reflect.DeepEqual(got, []string{strings.Repeat("a", 64)}) {
			t.Fatalf("references=%v err=%v", got, err)
		}
		if got := catalogGCSequence(t, st); got != want {
			t.Fatalf("sequence=%d want=%d", got, want)
		}
	}
}

func TestCatalogPackageGCFailureReturnsNoReferences(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"missing barrier", "DELETE FROM catalog_gc_barriers"},
		{"overflow", "UPDATE catalog_gc_barriers SET sequence = 9223372036854775807"},
		{"invalid reference", `UPDATE instances SET spec = '{invalid'`},
		{"read failure", "DROP TABLE instances"},
		{"write failure", `CREATE TRIGGER reject_gc AFTER UPDATE ON catalog_gc_barriers BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := catalogReferenceFixture(t)
			if err := st.DB().Exec(tc.sql).Error; err != nil {
				t.Fatal(err)
			}
			refs, err := st.CatalogPackageReferencesForGC(t.Context())
			if err == nil || refs != nil {
				t.Fatalf("references=%v err=%v", refs, err)
			}
			if tc.name != "missing barrier" {
				want := int64(0)
				if tc.name == "overflow" {
					want = math.MaxInt64
				}
				if got := catalogGCSequence(t, st); got != want {
					t.Fatalf("failed transaction changed sequence: %d", got)
				}
			}
		})
	}
	st := catalogReferenceFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if refs, err := st.CatalogPackageReferencesForGC(ctx); err == nil || refs != nil {
		t.Fatalf("cancellation returned references=%v err=%v", refs, err)
	}
	if catalogGCSequence(t, st) != 0 {
		t.Fatal("canceled transaction committed")
	}
}

func TestCatalogPackageGCUsesSchedulingDatabase(t *testing.T) {
	ordinary := catalogReferenceFixture(t)
	local := catalogReferenceFixture(t)
	// The ordinary pool must never substitute for a configured local-only pool.
	ordinary.schedulingDB = local.DB()
	t.Cleanup(func() { ordinary.schedulingDB = nil })
	if err := local.DB().Exec("DELETE FROM catalog_gc_barriers").Error; err != nil {
		t.Fatal(err)
	}
	if refs, err := ordinary.CatalogPackageReferencesForGC(t.Context()); err == nil || refs != nil {
		t.Fatalf("fell back to ordinary pool: %v %v", refs, err)
	}
	if catalogGCSequence(t, ordinary) != 0 {
		t.Fatal("ordinary pool authorized GC")
	}
}

func catalogGCSequence(t *testing.T, st *Store) int64 {
	t.Helper()
	var n int64
	if err := st.DB().Raw("SELECT sequence FROM catalog_gc_barriers WHERE id = 1").Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// The server committed but its acknowledgement was lost. This must never
// release a reference list for deletion, even though the sequence advanced.
func TestCatalogPackageGCUnknownCommitReturnsNoReferences(t *testing.T) {
	st := catalogReferenceFixture(t)
	pool, err := st.DB().DB()
	if err != nil {
		t.Fatal(err)
	}
	unknown := errors.New("injected lost commit acknowledgement")
	previous := st.db.Statement.ConnPool
	st.db.Statement.ConnPool = &catalogGCUnknownCommitPool{DB: pool, failure: unknown}
	t.Cleanup(func() { st.db.Statement.ConnPool = previous })
	refs, err := st.CatalogPackageReferencesForGC(t.Context())
	st.db.Statement.ConnPool = previous
	if !errors.Is(err, unknown) || refs != nil {
		t.Fatalf("unknown commit returned references=%v err=%v", refs, err)
	}
	if catalogGCSequence(t, st) != 1 {
		t.Fatal("fixture did not actually commit")
	}
	refs, err = st.CatalogPackageReferencesForGC(t.Context())
	if err != nil || !reflect.DeepEqual(refs, []string{strings.Repeat("a", 64)}) || catalogGCSequence(t, st) != 2 {
		t.Fatalf("fresh retry failed: references=%v err=%v", refs, err)
	}
}

func catalogReferenceFixture(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "refs.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, row := range []any{
		&InferenceModel{ID: 1, ModelID: "refs", PackageDigest: strings.Repeat("a", 64)},
		&InferenceSpecRow{ID: 1, ModelID: 1, Source: "user", Name: "refs", Image: "image", WeightSources: "[]", GPUs: "{}"},
		&Deployment{ID: 1, Name: "refs", CatalogSelectionSnapshot: "null"},
		&Instance{ID: 1, UUID: "refs", DeploymentID: 1, DesiredStateJSON: "null"},
	} {
		if err := st.DB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return st
}
