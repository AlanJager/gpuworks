package hub

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// proxiedURL names the archive by a host only the proxy reaches: loopback is
// never proxied.
func proxiedURL(origin string) string {
	return strings.Replace(origin, "127.0.0.1", "8.8.8.8", 1)
}

func archiveProxy(t *testing.T, origin string, st *store.Store) *atomic.Int64 {
	t.Helper()
	target, _ := url.Parse(origin)
	target.Path = ""
	var hits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "8.8.8.8:"+target.Port() {
			t.Errorf("proxy request=%s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		hits.Add(1)
		upstream, err := net.Dial("tcp", target.Host)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, conn); upstream.Close(); close(done) }()
		_, _ = io.Copy(conn, upstream)
		conn.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	setProxies(t, st, everyNode(proxy.URL, ""))
	return &hits
}

func TestImageArchiveRelayUsesGlobalProxy(t *testing.T) {
	h := newRelayHub(t, "file:hub_relay_proxy?mode=memory&cache=shared", "/image.tar")
	hits := archiveProxy(t, h.url, h.store)
	n, err := h.hub.CopyImageArchiveURL(context.Background(), proxiedURL(h.url), h.node.UUID, h.image.ImageRef, "proxy-transfer", nil)
	if err != nil || n != int64(len(h.archive)) {
		t.Fatalf("copy bytes=%d err=%v", n, err)
	}
	select {
	case body := <-h.loaded:
		if !bytes.Equal(body, h.archive) {
			t.Fatal("relayed archive changed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("archive not received")
	}
	if hits.Load() != 1 {
		t.Fatalf("relay proxy requests=%d, want 1", hits.Load())
	}
}

func TestImageArchiveAdmissionHEADUsesGlobalProxy(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_archive_head_proxy?mode=memory&cache=shared", nil)
	hits := archiveProxy(t, h.url, h.store)
	_, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceURL, ArchiveURL: proxiedURL(h.url)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-h.requests:
		if request.SizeBytes != h.size {
			t.Fatalf("size=%d want %d", request.SizeBytes, h.size)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent was not asked to download")
	}
	if hits.Load() != 1 {
		t.Fatalf("HEAD proxy requests=%d, want 1", hits.Load())
	}
}
