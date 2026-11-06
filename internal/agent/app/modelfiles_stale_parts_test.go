package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A platform download was cancelled (its resume sidecars kept) and the files
// were then finished with another tool, such as `hf download`. The next
// platform download takes the directory as it is: it removes the sidecars
// beside files complete at their listed size, downloads nothing, and the
// model is present - where it used to fail as "unfinished download".
func TestDownloadRepoClearsSidecarsLeftBesideFilesAnotherToolFinished(t *testing.T) {
	config := []byte(`{"model_type":"test"}`)
	first, second := []byte("first-shard-bytes"), []byte("second-shard")
	root := t.TempDir()
	model := filepath.Join(root, "model")
	for name, data := range map[string][]byte{"config.json": config, "model-00001-of-00002.safetensors": first, "model-00002-of-00002.safetensors": second} {
		if err := os.MkdirAll(model, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(model, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// What internal/agent/download leaves on a cancel of a ranged transfer.
	stale := filepath.Join(model, "model-00002-of-00002.safetensors")
	if err := os.WriteFile(stale+".part", second[:4], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale+".part.state", []byte(fmt.Sprintf(`{"url":"x","size":%d,"chunks":[2,4,6,8]}`, len(second))), 0o644); err != nil {
		t.Fatal(err)
	}
	var transfers atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/org/finished/tree/main" {
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model-00001-of-00002.safetensors","size":%d},{"type":"file","path":"model-00002-of-00002.safetensors","size":%d}]`, len(config), len(first), len(second))
			return
		}
		transfers.Add(1)
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()

	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	status := make(chan proto.ModelFileStatus, 16)
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			status <- value.(proto.ModelFileStatus)
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "stale-parts", Source: "huggingface", SourceID: "org/finished", Revision: "main",
		Endpoint: server.URL, TargetPath: "model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	n.reconcileModelFile(context.Background(), task)
	deadline := time.After(30 * time.Second)
	for {
		select {
		case got := <-status:
			if got.State == proto.MFFailed {
				t.Fatalf("download failed: %+v", got)
			}
			if got.State != proto.MFPresent {
				continue
			}
			if n := transfers.Load(); n != 0 {
				t.Fatalf("%d file requests; every file was already complete", n)
			}
			for _, gone := range []string{stale + ".part", stale + ".part.state"} {
				if _, err := os.Stat(gone); !os.IsNotExist(err) {
					t.Fatalf("%s still present: %v", filepath.Base(gone), err)
				}
			}
			if data, err := os.ReadFile(stale); err != nil || string(data) != string(second) {
				t.Fatalf("finished shard = %q, %v", data, err)
			}
			if _, ok := validCompletionMarker(model, task); !ok {
				t.Fatal("no valid completion marker")
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the model to be present")
		}
	}
}
