package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDownloadRepoRetriesListingBeforeReportingTerminalFailure(t *testing.T) {
	config, weights, vocab := []byte(`{"model_type":"test"}`), []byte("fixture-weights"), []byte("fixture-vocabulary")
	var listings atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			if listings.Add(1) == 1 {
				http.Error(w, "upstream temporarily unavailable", http.StatusBadGateway)
				return
			}
			writeModelScopeRetryManifest(t, w, r, config, weights, vocab)
		case "/models/org/model/resolve/" + modelDownloadRetryFixtureRevision + "/vocab.json":
			w.Header().Set("Content-Length", fmt.Sprint(len(vocab)))
			_, _ = w.Write(vocab)
		default:
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()
	n, task, target, statuses := newModelDownloadRetryFixture(t, server.URL, config, weights)
	n.downloadRepo(context.Background(), task, target, target)
	if got := lastModelFileStatus(t, statuses); got.State != proto.MFPresent || listings.Load() != 2 {
		t.Fatalf("listing did not recover: status=%+v attempts=%d", got, listings.Load())
	}
	for _, status := range *statuses {
		if status.State == proto.MFFailed {
			t.Fatalf("transient listing failure became terminal: %+v", status)
		}
		if status.TransferErrorCode != "" {
			t.Fatalf("in-flight retry got a final error code: %+v", status)
		}
	}
	got, err := os.ReadFile(filepath.Join(target, "vocab.json"))
	if err != nil || !bytes.Equal(got, vocab) {
		t.Fatalf("downloaded file = %q err=%v", got, err)
	}
}

func TestDownloadRepoReportsFinalListingCode(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{404, proto.ErrorModelDownloadSourceNotFound},
		{403, proto.ErrorModelDownloadSourceAccessDenied},
		{400, proto.ErrorModelDownloadSourceRejected},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var listings atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				listings.Add(1)
				http.Error(w, "provider evidence", tc.status)
			}))
			defer server.Close()
			n, task, target, statuses := newModelDownloadRetryFixture(t, server.URL, []byte(`{"model_type":"test"}`), []byte("weights"))
			task.TransferID = "listing-error-transfer"
			n.downloadRepo(context.Background(), task, target, target)
			got := lastModelFileStatus(t, statuses)
			if got.State != proto.MFFailed || got.TransferErrorCode != tc.code || got.TransferID != task.TransferID || listings.Load() != 1 {
				t.Fatalf("terminal report=%+v listings=%d", got, listings.Load())
			}
		})
	}
}
