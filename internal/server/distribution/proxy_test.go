package distribution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
)

func TestDistributionUsesCurrentProxyForIndexAndDownload(t *testing.T) {
	origin := newObjectStore(t)
	file := blob("releases/abc1234/gpuworks", []byte("verified release payload"))
	publish(t, testIndex("nightly"), origin)
	origin.put(file.Path, []byte("verified release payload"))
	target, _ := url.Parse(origin.URL())
	var hits atomic.Int64
	newProxy := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
		}))
	}
	first, second := newProxy(), newProxy()
	defer first.Close()
	defer second.Close()
	var raw atomic.Value
	raw.Store(proto.NetworkConfig{HTTPProxy: first.URL})
	transport := outbound.Transport(func() (proto.NetworkConfig, error) { return raw.Load().(proto.NetworkConfig), nil })
	defer transport.CloseIdleConnections()
	// Loopback is never proxied: the source is named by a host only the
	// proxies reach.
	source := strings.Replace(origin.URL(), "127.0.0.1", "releases.example", 1)
	client := &Client{Sources: []string{source}, Keys: testKeys(), HTTP: NewHTTPClient(transport.Proxy)}
	defer client.HTTP.CloseIdleConnections()
	fetched, err := client.FetchIndex(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("index proxy requests=%d, want 1", hits.Load())
	}
	raw.Store(proto.NetworkConfig{HTTPProxy: second.URL})
	first.Close()
	dest := filepath.Join(t.TempDir(), "gpuworks")
	if _, err = client.Download(context.Background(), fetched, file, dest, func(Progress) {}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "verified release payload" {
		t.Fatalf("download=%q err=%v", got, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("proxy requests after download=%d, want 2", hits.Load())
	}
	raw.Store(proto.NetworkConfig{HTTPProxy: "http://user:secret@%"})
	if _, err = client.FetchIndex(context.Background(), "nightly"); err == nil {
		t.Fatal("invalid configured proxy unexpectedly succeeded")
	} else if strings.Contains(err.Error(), "secret") {
		t.Fatalf("proxy credentials leaked: %v", err)
	}
}
