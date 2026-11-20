package api

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSettingErrorDiagnosticsKeepStatusAndPrivateValues(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var output bytes.Buffer
	h := &apiHandlers{d: Deps{Store: st, Log: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Put("/api/v1/settings/{key}", h.putSetting)
	call := func(ctx context.Context) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/DO-NOT-LOG-key", strings.NewReader(`"DO-NOT-LOG-value"`)).WithContext(ctx)
		r.Header.Set("X-Request-ID", "r-setting-diagnostic")
		r.Header.Set("Authorization", "Bearer DO-NOT-LOG-token")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	if w := call(t.Context()); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"status":"saved"}` {
		t.Fatalf("healthy result changed: %d %s", w.Code, w.Body)
	}
	if output.Len() != 0 {
		t.Fatalf("successful write logged error: %s", output.String())
	}
	for _, mode := range []string{"canceled", "deadline", "bad-connection", "business-closed"} {
		t.Run(mode, func(t *testing.T) {
			output.Reset()
			ctx := t.Context()
			cancel := func() {}
			switch mode {
			case "canceled":
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "deadline":
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			case "bad-connection":
				if err := st.DB().Callback().Update().Before("gorm:update").Register("test:setting-error", func(tx *gorm.DB) {
					if tx.Statement.Table == "settings" {
						tx.AddError(errors.Join(driver.ErrBadConn, errors.New("DO-NOT-LOG-error /private/authority")))
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer st.DB().Callback().Update().Remove("test:setting-error")
			case "business-closed":
				if err := st.InstallBusinessWriteBarrier(func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			defer cancel()
			w := call(ctx)
			want := http.StatusInternalServerError
			// A closed member, or one whose connection is still lost after the
			// whole retry budget, hands the write to another member.
			if mode == "business-closed" || mode == "bad-connection" {
				want = http.StatusServiceUnavailable
			}
			if w.Code != want {
				t.Fatalf("status changed: got%d want%d body%s", w.Code, want, w.Body)
			}
			var row map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &row); err != nil {
				t.Fatalf("missing structured error diagnostic: %s err=%v", output.String(), err)
			}
			if row["msg"] != "setting write failed" || row["request_id"] != "r-setting-diagnostic" || row["canceled"] != (mode == "canceled") || row["deadline_exceeded"] != (mode == "deadline") || row["business_writes_closed"] != (mode == "business-closed") || row["bad_connection"] != (mode == "bad-connection") || row["caller_canceled"] != (mode == "canceled") || row["caller_deadline_exceeded"] != (mode == "deadline") {
				t.Fatalf("wrong fixed flags/request: %s", output.String())
			}
			if strings.Contains(output.String(), "DO-NOT-LOG") || strings.Contains(output.String(), "/private/authority") {
				t.Fatalf("private diagnostic: %s", output.String())
			}
		})
	}
	h.d.Log = nil
	if w := call(t.Context()); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil logger changed closed-write status%d", w.Code)
	}
}
