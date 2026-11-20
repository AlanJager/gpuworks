//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaterialOwnerCancellationBeforeConsumerInstallReturnsNoReceipt(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "material-replicas"), 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(filepath.Join(dataDir, "material-replicas"), 2)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("logo received from a joined member")
	digest := materialTestDigest(body)
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	manifest := materialManifest{SourceMemberID: 1, Files: []materialManifestFile{{Class: MaterialModelLogo, Path: "model-logos/joined.png", SHA256: digest, Bytes: int64(len(body))}}}
	requests := newMaterialRequestTracker()
	entered := make(chan struct{})
	handler := requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		if err := materializeReceivedManifest(r.Context(), receiver, manifest); err != nil {
			http.Error(w, "material consumer paths could not be installed", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	receiver.mu.Lock() // Hold the installer after admission, before any consumer file work.
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler(response, httptest.NewRequest(http.MethodPost, "/materialize", nil))
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		receiver.mu.Unlock()
		t.Fatal("material installation did not enter the owner")
	}
	closing := make(chan error, 1)
	go func() { closing <- requests.withdraw(t.Context()) }()
	select {
	case <-requests.stop.Done():
	case <-time.After(3 * time.Second):
		receiver.mu.Unlock()
		t.Fatal("owner did not cancel admitted installation")
	}
	select {
	case err := <-closing:
		receiver.mu.Unlock()
		t.Fatalf("owner drained before installer returned: %v", err)
	default:
	}
	receiver.mu.Unlock()
	<-finished
	if err := <-closing; err != nil {
		t.Fatalf("owner did not drain cancelled installation: %v", err)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancelled materialization acknowledged installation: HTTP %d", response.Code)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "model-logos", "joined.png")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled installation published a consumer path: %v", err)
	}
}

func TestCancelledMaterialStageCannotLinkConsumerFile(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(rootPath, "stage"), []byte("complete staged bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := linkReceivedMaterial(ctx, root, "stage", "published"); !errors.Is(err, context.Canceled) {
		t.Fatalf("stage publication ignored owner cancellation: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "published")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled stage was published: %v", err)
	}
}
