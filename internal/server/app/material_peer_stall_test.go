//go:build linux && dqlite

package app

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func assertMaterialHandshakeBlackhole(t *testing.T, client *http.Client, digest string, size int64) {
	t.Helper()
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	go func() {
		conn, err := blackhole.Accept()
		if err == nil {
			defer conn.Close()
			<-t.Context().Done()
		}
	}()
	err = probeRemoteMaterialReplica(t.Context(), client, "https://"+blackhole.Addr().String(), digest, size, 7)
	if !peerUnreachable(err) {
		t.Fatalf("TLS handshake blackhole did not release material peer: %v", err)
	}
}

func assertMaterialBodyStall(t *testing.T, client *http.Client, serverTLS *tls.Config, digest string, size int64) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Gpuworks-Material-Member", "7")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	server.TLS = serverTLS.Clone()
	server.StartTLS()
	defer server.Close()
	err := verifyRemoteMaterialReplica(t.Context(), client, server.URL, digest, size, 7)
	if err == nil || peerUnreachable(err) {
		t.Fatalf("partial authenticated material response was treated as a copy or unavailable peer: %v", err)
	}
}
