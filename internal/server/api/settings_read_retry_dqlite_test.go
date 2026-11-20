//go:build linux && dqlite

package api

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// gatewayEOFPool drops the first n settings reads with the bare EOF the local
// gateway produces when it closes an in-flight read during a leadership
// transition. Every other statement and every later read reach the real
// replicated store unchanged.
type gatewayEOFPool struct {
	*sql.DB
	remaining atomic.Int32
}

func (p *gatewayEOFPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "settings") && p.remaining.Add(-1) >= 0 {
		return nil, io.EOF
	}
	return p.DB.QueryContext(ctx, query, args...)
}

var _ gorm.ConnPool = (*gatewayEOFPool)(nil)

func TestNativeGetSettingsRetriesGatewayEOFOnLeaderStepdown(t *testing.T) {
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
	if err := st.SetSetting("image_catalog_version", `"v5"`); err != nil {
		t.Fatal(err)
	}
	pool := &gatewayEOFPool{DB: db.SQL}
	pool.remaining.Store(2)
	gdb := st.DB()
	previous := gdb.Statement.ConnPool
	gdb.Statement.ConnPool = pool
	defer func() { gdb.Statement.ConnPool = previous }()

	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).getSettings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil).WithContext(ctx))
	gdb.Statement.ConnPool = previous
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"image_catalog_version":"v5"`) {
		t.Fatalf("settings lost across the retried read: %s", rec.Body.String())
	}
	if pool.remaining.Load() != -1 {
		t.Fatalf("injected gateway EOFs consumed=%d want exactly 2", -1-pool.remaining.Load())
	}
}
