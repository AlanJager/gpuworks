package engine

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
)

func TestReadOpenAPIPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"openapi":"3.1.0","paths":{"/v1/models":{},"/v1/chat/completions":{}}}`))
	}))
	defer server.Close()
	got := readOpenAPIPaths(context.Background(), server.Client(), serverPort(t, server))
	want := []string{"/v1/chat/completions", "/v1/models"}
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("paths = %v", got)
	}
}

func TestReadOpenAPIPathsUnknownWhenTheDocumentIsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer server.Close()
	if got := readOpenAPIPaths(context.Background(), server.Client(), serverPort(t, server)); got != nil {
		t.Fatalf("non-document = %v", *got)
	}
	if got := readOpenAPIPaths(context.Background(), server.Client(), 0); got != nil {
		t.Fatal("port 0 was probed")
	}
}

func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}
