//go:build linux && dqlite

package api

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// interruptedReadPool returns, for the first n settings reads, the failure a
// real three-member cluster produced on a large settings result during a
// leadership transfer: a dqlite driver.Error whose code and text are leftover
// stream bytes (observed code 3 "key"; the field saw "EOF").
type interruptedReadPool struct {
	*sql.DB
	remaining atomic.Int32
	failure   error
}

func (p *interruptedReadPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "settings") && p.remaining.Add(-1) >= 0 {
		return nil, p.failure
	}
	return p.DB.QueryContext(ctx, query, args...)
}

var _ gorm.ConnPool = (*interruptedReadPool)(nil)

func TestNativeGetSettingsRetriesInterruptedReplicatedRead(t *testing.T) {
	for _, failure := range []error{dqlitedriver.Error{Code: 3, Message: "key"}, dqlitedriver.Error{Code: 1, Message: "EOF"}} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := cluster.OpenDatabase(ctx, cluster.DatabaseConfig{Dir: t.TempDir(), Name: "gpuworks.db", Address: address})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			st, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if err := st.SetSetting("image_catalog_version", `"v6"`); err != nil {
				t.Fatal(err)
			}
			pool := &interruptedReadPool{DB: db.SQL, failure: failure}
			pool.remaining.Store(1)
			gdb := st.DB()
			previous := gdb.Statement.ConnPool
			gdb.Statement.ConnPool = pool
			defer func() { gdb.Statement.ConnPool = previous }()
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).getSettings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil).WithContext(ctx))
			gdb.Statement.ConnPool = previous
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"image_catalog_version":"v6"`) {
				t.Fatalf("status=%d body=%.200s", rec.Code, rec.Body.String())
			}
			if pool.remaining.Load() != -1 {
				t.Fatalf("injected interruptions consumed=%d want 1", -1-pool.remaining.Load())
			}
			// The pool must not keep a desynced connection: later reads succeed.
			for i := 0; i < 20; i++ {
				if got, err := st.AllSettings(); err != nil || got["image_catalog_version"] != `"v6"` {
					t.Fatalf("read %d after interruption: value=%q err=%v", i, got["image_catalog_version"], err)
				}
			}
		})
	}
}

// The classifier recognizes only dqlite failure frames.
func TestNativeReplicatedReadInterruptedClassification(t *testing.T) {
	if !store.ReplicatedReadInterrupted(dqlitedriver.Error{Code: 3, Message: "key"}) {
		t.Fatal("dqlite failure frame not recognized")
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, sql.ErrNoRows, store.ErrBusinessWritesClosed} {
		if store.ReplicatedReadInterrupted(err) {
			t.Fatalf("%v treated as an interrupted read", err)
		}
	}
}

// Writes do not inherit the read-only interruption class.
func TestNativeSettingsWriteDoesNotRetryInterruptedReadClass(t *testing.T) {
	if transientClusterReadError(dqlitedriver.Error{Code: 3, Message: "key"}) {
		t.Fatal("write retry classifier widened")
	}
}
