package api

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSetSettingWriteRetryRecoversBadConnection(t *testing.T) {
	var calls atomic.Int32
	err := setSettingWithWriteRetry(t.Context(), func(context.Context) error {
		if calls.Add(1) <= 2 {
			return driver.ErrBadConn
		}
		return nil
	}, 2*time.Second)
	if err != nil || calls.Load() != 3 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestSetSettingWriteRetryExhaustsBudgetWithLastError(t *testing.T) {
	started := time.Now()
	err := setSettingWithWriteRetry(t.Context(), func(context.Context) error { return driver.ErrBadConn }, 300*time.Millisecond)
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("exhausted retry returned %v", err)
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("budget not respected: %s", elapsed)
	}
}

func TestSetSettingWriteRetryKeepsNonTransientError(t *testing.T) {
	original := errors.New("UNIQUE constraint failed: settings.key")
	var calls atomic.Int32
	err := setSettingWithWriteRetry(t.Context(), func(context.Context) error { calls.Add(1); return original }, 2*time.Second)
	if !errors.Is(err, original) || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
	err = setSettingWithWriteRetry(t.Context(), func(context.Context) error { return store.ErrBusinessWritesClosed }, 2*time.Second)
	if !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("closed write admission was replaced: %v", err)
	}
}

func TestSetSettingWriteRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32
	started := time.Now()
	err := setSettingWithWriteRetry(ctx, func(context.Context) error {
		if calls.Add(1) == 2 {
			cancel()
		}
		return driver.ErrBadConn
	}, 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled retry waited %s", elapsed)
	}
}

// A leadership change can lose the acknowledgement of a commit that did
// happen. Repeating the same upsert must converge without a second row and
// only advance the per-setting notification revision.
func TestSetSettingWriteRetryConvergesAfterCommittedButUnacknowledgedWrite(t *testing.T) {
	st, err := store.Open("file:api_settings_write_retry_committed?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var calls atomic.Int32
	err = setSettingWithWriteRetry(t.Context(), func(ctx context.Context) error {
		if err := st.SetBusinessSetting(ctx, "test.ha.committed", `"v5"`); err != nil {
			return err
		}
		if calls.Add(1) == 1 {
			return driver.ErrBadConn
		}
		return nil
	}, 2*time.Second)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
	var rows int64
	if err := st.DB().Model(&store.Setting{}).Where("key = ?", "test.ha.committed").Count(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	got, err := st.AllSettings()
	if err != nil || got["test.ha.committed"] != `"v5"` {
		t.Fatalf("value=%q err=%v", got["test.ha.committed"], err)
	}
	var note store.CommittedNotification
	if err := st.DB().First(&note, "resource_id = ? AND topic = ?", "test.ha.committed", "setting").Error; err != nil || note.Revision != 2 {
		t.Fatalf("notification revision=%d err=%v", note.Revision, err)
	}
}

// failSettingsWrites injects driver-level failures into the first n settings
// row writes of a real store, inside the transaction so it rolls back.
func failSettingsWrites(t *testing.T, st *store.Store, n int32, err error) *atomic.Int32 {
	t.Helper()
	failed := new(atomic.Int32)
	inject := func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" && tx.Error == nil && failed.Load() < n {
			failed.Add(1)
			tx.AddError(err)
		}
	}
	const hook = "test:settings-write-retry"
	if err := st.DB().Callback().Update().After("gorm:update").Register(hook, inject); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Callback().Create().After("gorm:create").Register(hook, inject); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.DB().Callback().Update().Remove(hook)
		_ = st.DB().Callback().Create().Remove(hook)
	})
	return failed
}

func putSettingRequest(key, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/"+key, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("key", key)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestPutSettingRetriesBadConnectionDuringLeadershipChange(t *testing.T) {
	st, err := store.Open("file:api_settings_write_retry_put?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	failed := failSettingsWrites(t, st, 2, driver.ErrBadConn)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).putSetting(rec, putSettingRequest("test.ha.retry", `{"round":1}`))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"saved\"}\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if failed.Load() != 2 {
		t.Fatalf("injected failures=%d want 2", failed.Load())
	}
	got, err := st.AllSettings()
	if err != nil || got["test.ha.retry"] != `{"round":1}` {
		t.Fatalf("value=%q err=%v", got["test.ha.retry"], err)
	}
}

func TestPutSettingExhaustsWriteRetryWithLastError(t *testing.T) {
	st, err := store.Open("file:api_settings_write_retry_put_exhausted?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	failSettingsWrites(t, st, 1<<30, driver.ErrBadConn)
	started := time.Now()
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).putSetting(rec, putSettingRequest("test.ha.retry", `{"round":1}`))
	elapsed := time.Since(started)
	// Still no leader after the whole budget: 503 hands the idempotent write
	// to another member through the edge, keeping the last driver error.
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "{\"code\":\"SERVICE_UNAVAILABLE\",\"error\":\"driver: bad connection\"}\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if elapsed < settingsWriteRetryBudget-200*time.Millisecond || elapsed > settingsWriteRetryBudget+3*time.Second {
		t.Fatalf("retry budget not respected: elapsed=%s", elapsed)
	}
}

func TestPutSettingKeepsNonTransientWriteError(t *testing.T) {
	st, err := store.Open("file:api_settings_write_retry_put_plain?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	failed := failSettingsWrites(t, st, 1<<30, errors.New("disk I/O error"))
	started := time.Now()
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).putSetting(rec, putSettingRequest("test.ha.retry", `{"round":1}`))
	if rec.Code != http.StatusInternalServerError || failed.Load() != 1 || !strings.Contains(rec.Body.String(), "disk I/O error") {
		t.Fatalf("status=%d injections=%d body=%s", rec.Code, failed.Load(), rec.Body.String())
	}
	if time.Since(started) > time.Second {
		t.Fatal("non-transient error was retried")
	}
}

// One slow attempt must not extend the request: the budget bounds wall time,
// and the attempt itself runs under the same deadline.
func TestSetSettingWriteRetryBudgetBoundsSlowAttempts(t *testing.T) {
	started := time.Now()
	var calls atomic.Int32
	err := setSettingWithWriteRetry(t.Context(), func(ctx context.Context) error {
		calls.Add(1)
		select {
		case <-ctx.Done():
			return driver.ErrBadConn
		case <-time.After(250 * time.Millisecond):
			return driver.ErrBadConn
		}
	}, 600*time.Millisecond)
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 1300*time.Millisecond {
		t.Fatalf("slow attempts stretched the budget to %s (calls=%d)", elapsed, calls.Load())
	}
	started = time.Now()
	_ = setSettingWithWriteRetry(t.Context(), func(ctx context.Context) error {
		<-ctx.Done()
		return driver.ErrBadConn
	}, 300*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 1100*time.Millisecond {
		t.Fatalf("a hung attempt outlived the budget: %s", elapsed)
	}
}

// A stopped member's closure can arrive joined with a driver error; it is
// definitive and must not be retried.
func TestSetSettingWriteRetryReturnsJoinedClosureAtOnce(t *testing.T) {
	joined := errors.Join(store.ErrBusinessWritesClosed, driver.ErrBadConn)
	if !transientClusterReadError(joined) {
		t.Fatal("fixture no longer exercises the joined-error case")
	}
	var calls atomic.Int32
	started := time.Now()
	err := setSettingWithWriteRetry(t.Context(), func(context.Context) error { calls.Add(1); return joined }, 2*time.Second)
	if !errors.Is(err, store.ErrBusinessWritesClosed) || calls.Load() != 1 || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("calls=%d err=%v elapsed=%s", calls.Load(), err, time.Since(started))
	}
}

// The budget can expire inside a retry. That attempt then fails with the
// budget's own deadline, which must not replace the cluster error that made
// the write retry: the caller would answer 500 instead of handing over.
func TestSetSettingWriteRetryKeepsTheClusterErrorWhenTheBudgetCutsARetry(t *testing.T) {
	var calls atomic.Int32
	err := setSettingWithWriteRetry(t.Context(), func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return driver.ErrBadConn
		}
		<-ctx.Done()
		return ctx.Err()
	}, 150*time.Millisecond)
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("error = %v after %d attempts, want the cluster error that caused the retry", err, calls.Load())
	}
}

// A first attempt that is merely slow has no cluster error to report.
func TestSetSettingWriteRetryReportsTheBudgetForASlowFirstAttempt(t *testing.T) {
	err := setSettingWithWriteRetry(t.Context(), func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the budget's deadline", err)
	}
}
