package app

import (
	"context"
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

// A model download goes through the node's proxy except to the hosts the
// node's no_proxy lists (plus loopback and the server, which the agent adds),
// matched as Go matches NO_PROXY. A directory source is no exception.
func TestModelSourceClientSkipsTheProxyForNoProxyHosts(t *testing.T) {
	net := proto.NetworkConfig{HTTPProxy: "http://proxy.corp:3128", HTTPSProxy: "http://proxy.corp:3128", NoProxy: "files.corp,.mirror.corp"}
	for _, tc := range []struct {
		source, endpoint, target string
		direct                   bool
	}{
		{"url", "http://files.corp/models/qwen", "http://files.corp/models/qwen/config.json", true},
		{"huggingface", "https://hf.mirror.corp", "https://hf.mirror.corp/api/models/x", true},
		{"huggingface", "https://huggingface.co", "https://huggingface.co/api/models/x", false},
		{"url", "http://other.example/m", "http://other.example/m/config.json", false},
	} {
		client, err := modelSourceClient(tc.source, tc.endpoint, net)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, tc.target, nil)
		got, err := client.Transport.(*http.Transport).Proxy(req)
		if err != nil || (got == nil) != tc.direct {
			t.Errorf("%s: proxy = %v, err = %v; want direct=%v", tc.target, got, err, tc.direct)
		}
	}
}

// A directory source is downloaded through the node's proxy - the HTTP
// proxy for an http:// directory - even when the agent's own environment says
// NO_PROXY=*: an explicit proxy answers only to its own no_proxy.
func TestStateSyncModelDownloadSelectsHTTPProxyForHTTPSource(t *testing.T) {
	files := urlModelFiles()
	_, fixture, _ := newAutoindexFixture(t, "/models/proxy", files)
	fixtureURL, err := url.Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// Loopback is never proxied, so the task names a host only the proxy
	// knows how to reach.
	endpoint := "http://files.example" + fixtureURL.Path
	var proxyRequests atomic.Int64
	forward := http.DefaultTransport.(*http.Transport).Clone()
	forward.Proxy = nil
	defer forward.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		req := r.Clone(r.Context())
		req.RequestURI = ""
		if req.URL.Host == "files.example" {
			req.URL.Host, req.Host = fixtureURL.Host, fixtureURL.Host
		}
		response, err := forward.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxy.Close()
	wrongProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "HTTPS-only proxy must not receive HTTP model requests", http.StatusBadGateway)
	}))
	defer wrongProxy.Close()
	t.Setenv("NO_PROXY", "*")
	root := t.TempDir()
	node, _ := urlModelRuntime(t, root)
	final := make(chan proto.ModelFileStatus, 1)
	node.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			status := value.(proto.ModelFileStatus)
			if status.State == proto.MFPresent || status.State == proto.MFFailed || status.State == proto.MFInsufficientDisk {
				final <- status
			}
		}
		return nil
	})
	task := urlTask(endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node.OnStateSync(ctx, proto.DesiredState{Network: proto.NetworkConfig{HTTPProxy: proxy.URL, HTTPSProxy: wrongProxy.URL}, ModelFiles: []proto.ModelFileTask{task}})
	select {
	case status := <-final:
		if status.State != proto.MFPresent {
			t.Fatalf("HTTP proxy model download failed: %+v", status)
		}
	case <-ctx.Done():
		t.Fatal("model download did not finish")
	}
	if proxyRequests.Load() == 0 {
		t.Fatal("explicit HTTP proxy was bypassed by ambient NO_PROXY")
	}
	for name, file := range files {
		actual, err := os.ReadFile(filepath.Join(root, "model", name))
		if err != nil || string(actual) != file.body {
			t.Fatalf("downloaded %s = %q, err=%v", name, actual, err)
		}
	}
}
