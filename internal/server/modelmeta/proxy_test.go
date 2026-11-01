package modelmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestFetchUsesConfiguredClientForEveryMetadataRequest(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture-Proxy") != "yes" {
			http.Error(w, "proxy required", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models/acme/model/revision/main":
			w.Write([]byte(`{"sha":"pinned-commit"}`))
		case "/api/models/acme/model":
			w.Write([]byte(`{"author":"acme","createdAt":"2025-01-02T03:04:05Z"}`))
		case "/api/organizations/acme/avatar":
			w.Write([]byte(`{"avatarUrl":"https://cdn-avatars.example.test/uploads/acme.png"}`))
		case "/acme/model/resolve/main/config.json":
			w.Write([]byte(`{"model_type":"qwen3"}`))
		case "/acme/model/resolve/main/README.md":
			w.Write([]byte("# Through proxy"))
		case "/api/models/acme/model/tree/main":
			w.Write([]byte(`[{"type":"file","path":"model.safetensors","size":1234}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	target, _ := url.Parse(origin.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Fixture-Proxy", "yes")
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	defer client.CloseIdleConnections()
	meta, err := FetchWithClient(context.Background(), "huggingface", "acme/model", "", "", origin.URL, client)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ResolvedRevision != "pinned-commit" || meta.OriginCreatedAt == nil || meta.Arch != "qwen3" || meta.README != "# Through proxy" || meta.WeightBytes != 1234 || meta.LogoURL != "https://cdn-avatars.example.test/uploads/acme.png" {
		t.Fatalf("metadata did not consistently use configured proxy: %+v", meta)
	}
}
