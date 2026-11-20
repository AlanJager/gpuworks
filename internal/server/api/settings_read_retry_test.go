package api

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestTransientClusterReadErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"wrapped-eof", fmt.Errorf("read settings: %w", io.EOF), true},
		{"bad-connection", driver.ErrBadConn, true},
		// Only the driver's own sentinels count; a message that merely reads
		// like one does not (the replicated build has the no-leader case).
		{"text-only-no-leader", errors.New("no available dqlite leader server found"), false},
		{"text-only-eof", errors.New("EOF"), false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"constraint", errors.New("UNIQUE constraint failed: settings.key"), false},
		{"other", errors.New("disk I/O error"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientClusterReadError(tc.err); got != tc.want {
				t.Fatalf("transientClusterReadError(%v)=%t want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestAllSettingsReadRetryRecoversTransientEOF(t *testing.T) {
	want := map[string]string{"k": `"v"`}
	var calls atomic.Int32
	started := time.Now()
	got, err := allSettingsWithReadRetry(t.Context(), func() (map[string]string, error) {
		if calls.Add(1) <= 2 {
			return nil, io.EOF
		}
		return want, nil
	}, 2*time.Second)
	if err != nil || calls.Load() != 3 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
	if got["k"] != `"v"` {
		t.Fatalf("recovered settings changed: %v", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("two transient failures consumed %s", elapsed)
	}
}

func TestAllSettingsReadRetryExhaustsBudgetWithLastError(t *testing.T) {
	const budget = 300 * time.Millisecond
	var calls atomic.Int32
	started := time.Now()
	_, err := allSettingsWithReadRetry(t.Context(), func() (map[string]string, error) {
		return nil, fmt.Errorf("transient failure %d: %w", calls.Add(1), io.EOF)
	}, budget)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("transient failure %d", calls.Load())) {
		t.Fatalf("budget exhaustion did not return the last error: %v after %d calls", err, calls.Load())
	}
	if elapsed < budget || elapsed > budget+2*time.Second {
		t.Fatalf("retry budget %s not respected: elapsed=%s calls=%d", budget, elapsed, calls.Load())
	}
}

func TestAllSettingsReadRetryKeepsNonTransientError(t *testing.T) {
	original := errors.New("UNIQUE constraint failed: settings.key")
	var calls atomic.Int32
	_, err := allSettingsWithReadRetry(t.Context(), func() (map[string]string, error) {
		calls.Add(1)
		return nil, original
	}, 2*time.Second)
	if !errors.Is(err, original) || calls.Load() != 1 {
		t.Fatalf("non-transient error retried or replaced: calls=%d err=%v", calls.Load(), err)
	}
}

func TestAllSettingsReadRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32
	started := time.Now()
	_, err := allSettingsWithReadRetry(ctx, func() (map[string]string, error) {
		if calls.Add(1) == 2 {
			cancel()
		}
		return nil, io.EOF
	}, 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled retry waited %s", elapsed)
	}
}

// failSettingsQueries injects driver-level failures into the first n settings
// reads of a real store, at the same boundary where a closing gateway drops
// them.
func failSettingsQueries(t *testing.T, st *store.Store, n int32, err error) *atomic.Int32 {
	t.Helper()
	failed := new(atomic.Int32)
	const hook = "test:settings-read-retry"
	if err := st.DB().Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" && tx.Error == nil && failed.Load() < n {
			failed.Add(1)
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(hook) })
	return failed
}

func TestGetSettingsRetriesTransientReadFailure(t *testing.T) {
	st, err := store.Open("file:api_settings_read_retry?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSetting("image_catalog_version", `"v4"`); err != nil {
		t.Fatal(err)
	}
	failed := failSettingsQueries(t, st, 2, io.EOF)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).getSettings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v; body=%s", err, rec.Body.String())
	}
	if string(got["image_catalog_version"]) != `"v4"` {
		t.Fatalf("recovered settings changed: %s", got["image_catalog_version"])
	}
	if failed.Load() != 2 {
		t.Fatalf("injected failures=%d want 2", failed.Load())
	}
}

func TestGetSettingsExhaustsRetryBudgetWithLastError(t *testing.T) {
	st, err := store.Open("file:api_settings_read_retry_exhausted?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	failSettingsQueries(t, st, 1<<30, io.EOF)
	started := time.Now()
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).getSettings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	elapsed := time.Since(started)
	// A connection still lost after the whole budget is answered 503: the
	// edge retries only 502 and 503, so another member can serve the read.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The response keeps the last driver error, in the shape the incident
	// accesslog recorded, instead of a synthetic timeout.
	if rec.Body.String() != "{\"code\":\"SERVICE_UNAVAILABLE\",\"error\":\"EOF\"}\n" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	if elapsed < store.ClusterMembershipGrantTimeout || elapsed > store.ClusterMembershipGrantTimeout+2*time.Second {
		t.Fatalf("retry budget not respected: elapsed=%s", elapsed)
	}
}

func TestGetSettingsKeepsNonTransientReadError(t *testing.T) {
	st, err := store.Open("file:api_settings_read_retry_plain?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	original := errors.New("disk I/O error")
	failed := failSettingsQueries(t, st, 1<<30, original)
	started := time.Now()
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).getSettings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusInternalServerError || failed.Load() != 1 {
		t.Fatalf("status=%d injections=%d body=%s", rec.Code, failed.Load(), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disk I/O error") {
		t.Fatalf("non-transient error replaced: %s", rec.Body.String())
	}
	if time.Since(started) > time.Second {
		t.Fatal("non-transient error was retried")
	}
}
