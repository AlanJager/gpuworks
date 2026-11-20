//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type blockingMaterialReceiptWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *blockingMaterialReceiptWriter) WriteHeader(status int) {
	close(w.entered)
	<-w.release
	w.ResponseRecorder.WriteHeader(status)
}

func TestMaterialReceiptWriteDoesNotBlockOwnerClose(t *testing.T) {
	requests := newMaterialRequestTracker()
	w := &blockingMaterialReceiptWriter{
		ResponseRecorder: httptest.NewRecorder(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	request := httptest.NewRequest(http.MethodPost, "/materialize", nil)
	handler := requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		writeMaterialManifestReceipt(w, r, requests, 1, 2, "digest", nil, "")
	})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler(w, request)
	}()
	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("receipt writer was not reached")
	}
	closed := make(chan struct{})
	go func() {
		requests.closeAdmissions()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		close(w.release)
		t.Fatal("blocked response writer held the owner cancellation lock")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := requests.wait(ctx); err != context.DeadlineExceeded {
		close(w.release)
		t.Fatalf("owner drained before the response writer returned: %v", err)
	}
	close(w.release)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("released response writer did not complete")
	}
	if err := requests.wait(t.Context()); err != nil {
		t.Fatalf("completed handler remained tracked: %v", err)
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("receipt decided before owner close was lost: HTTP %d", w.Code)
	}
}

func TestMaterialReceiptConfirmFailureStripsSuccessHeaders(t *testing.T) {
	requests := newMaterialRequestTracker()
	w := httptest.NewRecorder()
	writeMaterialManifestReceipt(w, httptest.NewRequest(http.MethodPost, "/materialize", nil), requests, 1, 2, "digest", func() error {
		return errors.New("changed")
	}, "material manifest changed during installation")
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "material manifest changed during installation\n" {
		t.Fatalf("confirm failure emitted a success receipt: HTTP %d body=%q headers=%v", w.Code, w.Body.String(), w.Header())
	}
	for _, header := range []string{"X-Gpuworks-Material-Digest", "X-Gpuworks-Material-Member", "X-Gpuworks-Material-Source"} {
		if w.Header().Get(header) != "" {
			t.Fatalf("confirm failure retained %s=%q", header, w.Header().Get(header))
		}
	}
}

func TestMaterialReceiptAfterOwnerCloseReturnsUnavailable(t *testing.T) {
	requests := newMaterialRequestTracker()
	requests.closeAdmissions()
	w := httptest.NewRecorder()
	writeMaterialManifestReceipt(w, httptest.NewRequest(http.MethodPost, "/materialize", nil), requests, 1, 2, "digest", nil, "")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("X-Gpuworks-Material-Digest") != "" {
		t.Fatalf("closed owner emitted a success receipt: HTTP %d headers=%v", w.Code, w.Header())
	}
}
