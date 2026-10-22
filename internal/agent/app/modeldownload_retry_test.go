package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

const modelDownloadRetryFixtureRevision = "25d1d1fa49de445ab1f3a41f3537d7dad222826e"

func TestDownloadRepoRetriesInterruptedModelScopeFile(t *testing.T) {
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("fixture-weights")
	vocab := bytes.Repeat([]byte("modelscope-vocabulary\n"), 2048)

	var transfers atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			writeModelScopeRetryManifest(t, w, r, config, weights, vocab)
		case "/models/org/model/resolve/" + modelDownloadRetryFixtureRevision + "/vocab.json":
			// download.Download first probes range support. ModelScope may ignore
			// that Range and return a normal 200 response, which exercises the
			// same single-stream path as the live vocab.json failure.
			if r.Header.Get("Range") == "bytes=0-0" {
				w.Header().Set("Content-Length", fmt.Sprint(len(vocab)))
				_, _ = w.Write(vocab)
				return
			}
			attempt := transfers.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(vocab)))
			w.WriteHeader(http.StatusOK)
			if attempt <= 2 {
				// Returning before Content-Length bytes have been written makes the
				// real net/http client observe io.ErrUnexpectedEOF.
				_, _ = w.Write(vocab[:len(vocab)/3])
				return
			}
			_, _ = w.Write(vocab)
		default:
			http.Error(w, "unexpected fixture request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	n, task, target, statuses := newModelDownloadRetryFixture(t, server.URL, config, weights)
	n.downloadRepo(context.Background(), task, target, target)

	got := lastModelFileStatus(t, statuses)
	if got.State != proto.MFPresent || got.Revision != modelDownloadRetryFixtureRevision {
		t.Fatalf("final status = %+v", got)
	}
	if count := transfers.Load(); count != 3 {
		t.Fatalf("vocab transfer attempts = %d, want 3", count)
	}
	downloaded, err := os.ReadFile(filepath.Join(target, "vocab.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded, vocab) {
		t.Fatalf("downloaded vocab differs: got %d bytes, want %d", len(downloaded), len(vocab))
	}
}

func TestDownloadRepoDoesNotRetryPermanentHTTPError(t *testing.T) {
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("fixture-weights")
	vocab := []byte("fixture-vocabulary")

	var transfers atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			writeModelScopeRetryManifest(t, w, r, config, weights, vocab)
		case "/models/org/model/resolve/" + modelDownloadRetryFixtureRevision + "/vocab.json":
			transfers.Add(1)
			http.Error(w, "access denied", http.StatusForbidden)
		default:
			http.Error(w, "unexpected fixture request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	n, task, target, statuses := newModelDownloadRetryFixture(t, server.URL, config, weights)
	n.downloadRepo(context.Background(), task, target, target)

	got := lastModelFileStatus(t, statuses)
	if got.State != proto.MFFailed || !strings.Contains(got.Message, "unexpected probe status 403") {
		t.Fatalf("final status = %+v", got)
	}
	if count := transfers.Load(); count != 1 {
		t.Fatalf("permanent HTTP error was requested %d times, want 1", count)
	}
}

func TestDownloadRepoCancellationStopsRetry(t *testing.T) {
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("fixture-weights")
	vocab := bytes.Repeat([]byte("vocab"), 4096)

	started := make(chan struct{})
	var startedOnce sync.Once
	var transfers atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			writeModelScopeRetryManifest(t, w, r, config, weights, vocab)
		case "/models/org/model/resolve/" + modelDownloadRetryFixtureRevision + "/vocab.json":
			if r.Header.Get("Range") == "bytes=0-0" {
				w.Header().Set("Content-Length", fmt.Sprint(len(vocab)))
				_, _ = w.Write(vocab)
				return
			}
			transfers.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(vocab)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(vocab[:32])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			startedOnce.Do(func() { close(started) })
			<-r.Context().Done()
		default:
			http.Error(w, "unexpected fixture request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	n, task, target, statuses := newModelDownloadRetryFixture(t, server.URL, config, weights)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.downloadRepo(ctx, task, target, target)
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for the interrupted transfer to start")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("downloadRepo did not return after cancellation")
	}
	if count := transfers.Load(); count != 1 {
		t.Fatalf("canceled transfer was requested %d times, want 1", count)
	}
	if got := lastModelFileStatus(t, statuses); got.State == proto.MFFailed {
		t.Fatalf("agent shutdown was reported as a model failure: %+v", got)
	}
}

func newModelDownloadRetryFixture(t *testing.T, endpoint string, config, weights []byte) (*NodeRuntime, proto.ModelFileTask, string, *[]proto.ModelFileStatus) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "model")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "model.safetensors"), weights, 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		modelsDir:   root,
		downloading: map[string]*modelDownload{},
	}
	statuses := &[]proto.ModelFileStatus{}
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			*statuses = append(*statuses, value.(proto.ModelFileStatus))
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "retry-fixture", Source: modelrepo.SourceMS, SourceID: "org/model", Revision: "master",
		Endpoint: endpoint, TargetPath: "model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	return n, task, target, statuses
}

func lastModelFileStatus(t *testing.T, statuses *[]proto.ModelFileStatus) proto.ModelFileStatus {
	t.Helper()
	for i := len(*statuses) - 1; i >= 0; i-- {
		if (*statuses)[i].Key == "retry-fixture" {
			return (*statuses)[i]
		}
	}
	t.Fatal("no model file status was recorded")
	return proto.ModelFileStatus{}
}

func writeModelScopeRetryManifest(t *testing.T, w http.ResponseWriter, r *http.Request, config, weights, vocab []byte) {
	t.Helper()
	if r.URL.Query().Get("Recursive") != "true" || r.URL.Query().Get("Revision") != "master" {
		t.Errorf("ModelScope list query = %q, want Recursive=true and Revision=master", r.URL.RawQuery)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w,
		`{"Code":200,"Data":{"Files":[`+
			`{"Path":"config.json","Size":%d,"Type":"blob","Revision":"%s"},`+
			`{"Path":"model.safetensors","Size":%d,"Type":"blob","Revision":"%s"},`+
			`{"Path":"vocab.json","Size":%d,"Type":"blob","Revision":"%s"}`+
			`]}}`,
		len(config), modelDownloadRetryFixtureRevision,
		len(weights), modelDownloadRetryFixtureRevision,
		len(vocab), modelDownloadRetryFixtureRevision,
	)
}
