package api

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// The management API's JSON is gzipped for a client that accepts it and
// plain for one that does not, with the same bytes once decompressed.
func TestManagementJSONIsCompressed(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-compress-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := routerAuthStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{AuthSigner: signer, Store: st})
	get := func(encoding string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/engines", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if encoding != "" {
			req.Header.Set("Accept-Encoding", encoding)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	plain := get("")
	if plain.Code != http.StatusOK || plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("plain = %d %q", plain.Code, plain.Header().Get("Content-Encoding"))
	}
	zipped := get("gzip, deflate, br")
	if zipped.Code != http.StatusOK || zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("gzip = %d %v", zipped.Code, zipped.Header())
	}
	reader, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != plain.Body.String() {
		t.Fatalf("decompressed body differs:\n%s\nwant\n%s", body, plain.Body.String())
	}
	if zipped.Body.Len() >= len(body) {
		t.Fatalf("compressed %d bytes is not smaller than %d", zipped.Body.Len(), len(body))
	}
	// Errors are JSON too, and an unauthenticated one is still refused.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/engines", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
}

// What the middleware must not break: a websocket upgrade from a browser
// that offers gzip (the web terminal), and a stream whose events have to
// arrive as they are written.
func TestCompressionPassesUpgradesAndStreams(t *testing.T) {
	r := chi.NewRouter()
	r.Use(compressJSON)
	r.Get("/ws", func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		kind, data, err := conn.Read(req.Context())
		if err == nil {
			_ = conn.Write(req.Context(), kind, data)
		}
	})
	release := make(chan struct{})
	r.Get("/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	})
	server := httptest.NewServer(r)
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Accept-Encoding": []string{"gzip, deflate, br"}},
	})
	if err != nil {
		t.Fatalf("websocket upgrade through the middleware: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.Read(ctx); err != nil || string(data) != "ping" {
		t.Fatalf("echo = %q %v", data, err)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("an event stream was compressed: %v", resp.Header)
	}
	// The first event arrives while the handler is still writing.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event = %q %v", line, err)
	}
}
