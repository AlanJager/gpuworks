package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Exercise the wire update through a real model download: an absent network
// keeps the selected proxy, while an explicit empty network clears it.
func TestNetworkUpdateChangesSubsequentDownloads(t *testing.T) {
	t.Setenv("NO_PROXY", "*")
	files := urlModelFiles()
	_, endpoint, _ := newAutoindexFixture(t, "/models/network-update", files)
	fixture, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	// Loopback is never proxied: a proxied download names a host only the
	// proxy knows how to reach.
	proxiedEndpoint := "http://files.example" + fixture.Path
	forward := http.DefaultTransport.(*http.Transport).Clone()
	forward.Proxy = nil
	defer forward.CloseIdleConnections()
	var requests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		req := r.Clone(r.Context())
		req.RequestURI = ""
		if req.URL.Host == "files.example" {
			req.URL.Host, req.Host = fixture.Host, fixture.Host
		}
		resp, err := forward.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	root := t.TempDir()
	node, _ := urlModelRuntime(t, root)
	final := make(chan proto.ModelFileStatus, 8)
	node.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			status := value.(proto.ModelFileStatus)
			if status.State == proto.MFPresent || status.State == proto.MFFailed || status.State == proto.MFInsufficientDisk {
				final <- status
			}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for i, step := range []struct {
		wire    string
		proxied bool
	}{
		{fmt.Sprintf(`{"network":{"http_proxy":%q}}`, proxy.URL), true},
		{`{}`, true},
		{`{"network":{}}`, false},
	} {
		var update proto.StateUpdate
		if err := json.Unmarshal([]byte(step.wire), &update); err != nil {
			t.Fatal(err)
		}
		if node.OnStateUpdate(ctx, update) {
			t.Fatal("network-only update requested an image inventory refresh")
		}
		if node.ModelsRoot() != root {
			t.Fatal("network-only update changed the models root")
		}
		before := requests.Load()
		task := urlTask(endpoint)
		if step.proxied {
			task = urlTask(proxiedEndpoint)
		}
		task.Key = fmt.Sprintf("31-%d", i+1)
		task.TargetPath = fmt.Sprintf("model-%d", i)
		node.OnStateUpdate(ctx, proto.StateUpdate{ModelFiles: []proto.ModelFileTask{task}})
		select {
		case status := <-final:
			if status.State != proto.MFPresent || status.Key != task.Key {
				t.Fatalf("step %d download: %+v", i, status)
			}
		case <-ctx.Done():
			t.Fatalf("step %d download: %v", i, ctx.Err())
		}
		if used := requests.Load() > before; used != step.proxied {
			t.Fatalf("step %d used proxy=%v, want %v", i, used, step.proxied)
		}
		for name, file := range files {
			got, err := os.ReadFile(filepath.Join(root, task.TargetPath, name))
			if err != nil || string(got) != file.body {
				t.Fatalf("step %d file %s = %q, err=%v", i, name, got, err)
			}
		}
	}
}
