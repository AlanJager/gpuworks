//go:build linux && dqlite

package app

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterialRouteMissingNamesThePageAndRefusedDatabase(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyConnection:   func(tls.ConnectionState) error { return nil },
	}}}
	source := filepath.Join(t.TempDir(), "bytes")
	body := []byte("catalog package bytes")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := sendMaterialReplica(t.Context(), client, server.URL, source, materialTestDigest(body), 7)
	if err == nil || !strings.Contains(err.Error(), "material route is not registered: HTTP 404") || !strings.Contains(err.Error(), "404 page not found") {
		t.Fatalf("missing material route error = %v", err)
	}
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	openAddr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	explained := explainMemberReachability(t.Context(), err, openAddr)
	if explained == nil || !errors.Is(explained, err) || !strings.Contains(explained.Error(), "connection refused") || !strings.Contains(explained.Error(), openAddr) {
		t.Fatalf("refused database annotation = %v", explained)
	}
	if unavailableMaterialPeer(explained) {
		t.Fatal("HTTP 404 with a refused database port was treated as an unreachable peer")
	}
}

func TestMaterialReceiverRejectionKeepsItsStatus(t *testing.T) {
	err := materialStatusError(statusResponse(http.StatusUnprocessableEntity), []byte("no"))
	if err == nil || err.Error() != "material receiver rejected transfer: HTTP 422" {
		t.Fatalf("receiver rejection = %v", err)
	}
}
