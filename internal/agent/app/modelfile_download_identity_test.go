package app

import (
	"context"
	"fmt"
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
)

// gatedModelSource serves a two-file repository like the hub does (the tree
// listing, then the files by revision) but holds every request for the weights
// until the test releases it, so a task can change while the download runs.
// Released as a failure it answers the way a hub does for a file that vanished.
type gatedModelSource struct {
	URL      string
	reached  chan struct{}
	gate     chan struct{}
	failing  atomic.Bool
	once     sync.Once
	reachOne sync.Once
	paths    sync.Map
}

func newGatedModelSource(t *testing.T) *gatedModelSource {
	t.Helper()
	config := []byte(`{"architectures":["Qwen3ForCausalLM"]}`)
	weights := []byte("weights")
	g := &gatedModelSource{reached: make(chan struct{}), gate: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.paths.Store(r.URL.Path, true)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/org/model/tree/"):
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model.safetensors","size":%d}]`, len(config), len(weights))
		case strings.HasSuffix(r.URL.Path, "/config.json"):
			http.ServeContent(w, r, "config.json", time.Time{}, strings.NewReader(string(config)))
		case strings.HasSuffix(r.URL.Path, "/model.safetensors"):
			g.reachOne.Do(func() { close(g.reached) })
			select {
			case <-g.gate:
			case <-r.Context().Done():
				return
			}
			if g.failing.Load() {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, "model.safetensors", time.Time{}, strings.NewReader(string(weights)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		g.release(false)
		server.Close()
	})
	g.URL = server.URL
	return g
}

func (g *gatedModelSource) release(fail bool) {
	g.once.Do(func() {
		g.failing.Store(fail)
		close(g.gate)
	})
}

func (g *gatedModelSource) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the download never asked for the weights")
	}
}

func (g *gatedModelSource) served(path string) bool {
	_, ok := g.paths.Load(path)
	return ok
}

func (r *storageTestRuntime) fileStatuses(key string) []proto.ModelFileStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []proto.ModelFileStatus
	for _, st := range r.files {
		if st.Key == key {
			out = append(out, st)
		}
	}
	return out
}

func downloadTask(source *gatedModelSource, transferID string) proto.ModelFileTask {
	task := modelWriterTask("download", "org/model", proto.ModelAcquireDownload, "", source.URL)
	task.TransferID = transferID
	return task
}

func activeWriter(t *testing.T, r *storageTestRuntime, key string) *modelDownload {
	t.Helper()
	r.dlMu.Lock()
	defer r.dlMu.Unlock()
	active := r.downloading[key]
	if active == nil {
		t.Fatalf("no writer registered for %s", key)
	}
	return active
}

// The task for a running download changes in ways that do not make it another
// transfer: a rate limit, a new address list, another endpoint. The download
// must keep running and still report how it ends, whether or not the server
// names its downloads.
func TestRunningDownloadReportsItsEndAfterTaskParametersChange(t *testing.T) {
	for _, transferID := range []string{"download-1", ""} {
		for _, ending := range []string{proto.MFPresent, proto.MFFailed} {
			t.Run(fmt.Sprintf("transfer %q ends %s", transferID, ending), func(t *testing.T) {
				r := newStorageTestRuntime(t)
				source := newGatedModelSource(t)
				task := downloadTask(source, transferID)
				r.reconcileModelFile(context.Background(), task)
				source.waitReached(t)
				running := activeWriter(t, r, task.Key)

				changed := task
				changed.RateLimitKBs = 256
				changed.Endpoint = source.URL + "/"
				changed.Candidates = []proto.ModelFetchCandidate{{Base: source.URL}}
				reconcileModelFileWithoutBlocking(t, r, changed)
				again := changed
				again.RateLimitKBs = 0
				again.Candidates = nil
				reconcileModelFileWithoutBlocking(t, r, again)

				if got := activeWriter(t, r, task.Key); got != running {
					t.Fatal("a parameter change replaced the running download")
				}
				select {
				case <-running.done:
					t.Fatal("a parameter change stopped the running download")
				default:
				}
				assertNoModelTaskWaiting(t, r)

				source.release(ending == proto.MFFailed)
				got := r.waitFile(t, task.Key, ending)
				if got.TransferID != transferID || got.Acquisition != proto.ModelAcquireDownload {
					t.Fatalf("final status = %+v, want a download report for %q", got, transferID)
				}
				if ending == proto.MFPresent {
					data, err := os.ReadFile(filepath.Join(r.ModelsRoot(), "org", "model", "model.safetensors"))
					if err != nil || string(data) != "weights" {
						t.Fatalf("downloaded weights = %q, err=%v", data, err)
					}
				} else if got.Message == "" {
					t.Fatalf("failure carries no message: %+v", got)
				}
				for _, st := range r.fileStatuses(task.Key) {
					if st.TransferID != transferID {
						t.Fatalf("status %+v does not name the download %q", st, transferID)
					}
				}
				select {
				case <-running.done:
				case <-time.After(5 * time.Second):
					t.Fatal("the download did not return")
				}
			})
		}
	}
}

// A download the server prepared anew (a retry, another revision) is another
// transfer: the old one stops, and only the new one reports.
func TestNewDownloadPreparationReplacesTheRunningDownload(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := newGatedModelSource(t)
	first := downloadTask(source, "download-1")
	r.reconcileModelFile(context.Background(), first)
	source.waitReached(t)
	old := activeWriter(t, r, first.Key)

	second := downloadTask(source, "download-2")
	second.Revision = "next"
	reconcileModelFileWithoutBlocking(t, r, second)
	select {
	case <-old.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the replaced download kept running")
	}

	source.release(false)
	got := r.waitFile(t, first.Key, proto.MFPresent)
	if got.TransferID != "download-2" {
		t.Fatalf("final status = %+v, want the replacement download-2", got)
	}
	if !source.served("/org/model/resolve/next/model.safetensors") {
		t.Fatal("the replacement did not fetch the revision it was prepared for")
	}
	for _, st := range r.fileStatuses(first.Key) {
		if st.State == proto.MFPresent && st.TransferID != "download-2" {
			t.Fatalf("the replaced download reported %+v", st)
		}
	}
	assertNoModelTaskWaiting(t, r)
}

// A task that withdraws the model stops the download writing for it; neither
// a completion nor a failure of that download is reported afterwards.
func TestWithdrawnDownloadStopsAndReportsNothing(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := newGatedModelSource(t)
	task := downloadTask(source, "download-1")
	r.reconcileModelFile(context.Background(), task)
	source.waitReached(t)
	running := activeWriter(t, r, task.Key)
	before := len(r.fileStatuses(task.Key))

	absent := task
	absent.DesiredState = "absent"
	reconcileModelFileWithoutBlocking(t, r, absent)
	select {
	case <-running.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the withdrawn download kept running")
	}
	source.release(true)
	for _, st := range r.fileStatuses(task.Key)[before:] {
		if st.State == proto.MFPresent || st.State == proto.MFFailed {
			t.Fatalf("the withdrawn download reported %+v", st)
		}
	}
	assertNoModelTaskWaiting(t, r)
}
