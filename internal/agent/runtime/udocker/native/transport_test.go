package native

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A registry or CDN that offers HTTP/2 must still be spoken to over HTTP/1.1:
// the parts of a split blob, and a reconnect after a bad connection, each need
// a TCP connection of their own (see NewClientForPlatform).
func TestClientOpensOneConnectionPerConcurrentBlobRequest(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}
	protos := map[string]bool{}
	arrived := make(chan struct{}, 4)
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		protos[r.Proto] = true
		mu.Unlock()
		arrived <- struct{}{}
		<-release // hold every request open together
		w.Write([]byte("ok"))
	}))
	server.EnableHTTP2 = true // offered via ALPN, as a CDN does
	server.StartTLS()
	defer server.Close()

	client := NewClient()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport := client.HTTP.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = pool

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			resp, err := client.HTTP.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			close(release)
			wg.Wait()
			t.Fatalf("only %d of 4 requests reached the server", i)
		}
	}
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(protos) != 1 || !protos["HTTP/1.1"] {
		t.Fatalf("protocols = %v, want only HTTP/1.1", protos)
	}
	if len(conns) != 4 {
		t.Fatalf("4 concurrent requests shared %d connection(s): %v", len(conns), conns)
	}
}
