package modelfetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// gatedHub answers as Hugging Face does for a gated repository: the API and
// resolve want the token, resolve sends a weight file to the hub's CDN (a
// subdomain) and config.json to a signed storage URL on another domain. Every
// host resolves to this one server; it records the Authorization header each
// host was sent.
type gatedHub struct {
	mu   sync.Mutex
	seen map[string][]string
}

func (h *gatedHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.Host)
	if host == "" {
		host = r.Host
	}
	h.mu.Lock()
	h.seen[host] = append(h.seen[host], r.Header.Get("Authorization"))
	h.mu.Unlock()
	authorized := r.Header.Get("Authorization") == "Bearer hf_secret"
	switch {
	case host == "mirror.test":
		// A mirror that does not support gated repositories.
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	case host == "hub.test" && strings.HasPrefix(r.URL.Path, "/api/models/gated/model/tree/"):
		if !authorized {
			w.Header().Set("X-Error-Code", "GatedRepo")
			http.Error(w, `{"error":"Access to model gated/model is restricted."}`, http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `[{"type":"file","path":"config.json","size":2},{"type":"file","path":"model.safetensors","size":4}]`)
	case host == "hub.test" && strings.Contains(r.URL.Path, "/resolve/"):
		if !authorized {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		target := "https://storage.other/signed/" + name
		if strings.HasSuffix(name, ".safetensors") {
			target = "https://cdn.hub.test/blob/" + name
		}
		http.Redirect(w, r, target, http.StatusFound)
	case host == "cdn.hub.test" || host == "storage.other":
		body := "{}"
		if strings.HasSuffix(r.URL.Path, ".safetensors") {
			body = "wwww"
		}
		http.ServeContent(w, r, "f", time.Time{}, strings.NewReader(body))
	default:
		http.NotFound(w, r)
	}
}

func (h *gatedHub) headers(host string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen[host]...)
}

func TestTheTokenGoesToTheHubAndItsCDNOnly(t *testing.T) {
	hub := &gatedHub{seen: map[string][]string{}}
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	address := server.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a test server
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
	_, port, _ := net.SplitHostPort(address)
	f, err := New(Config{
		Source: modelrepo.SourceHF, Repo: "gated/model",
		Candidates: []proto.ModelFetchCandidate{{Base: "https://mirror.test:" + port}, {Base: "https://hub.test:" + port, Upstream: true}},
		Client:     func(string) (*http.Client, error) { return client, nil },
		Retry:      RetryPolicy{MaxAttempts: 2},
		Logins:     []modelrepo.Login{{Host: "hub.test:" + port, Secret: "hf_secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := fetchAll(t, f, commit, dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, map[string][]byte{"config.json": []byte("{}"), "model.safetensors": []byte("wwww")})
	for host, want := range map[string]string{"mirror.test": "", "storage.other": "", "hub.test": "Bearer hf_secret", "cdn.hub.test": "Bearer hf_secret"} {
		seen := hub.headers(host)
		if len(seen) == 0 {
			t.Fatalf("%s was never asked", host)
		}
		for _, got := range seen {
			if got != want {
				t.Fatalf("%s was sent Authorization %q, want %q (all: %v)", host, got, want, seen)
			}
		}
	}
}

// A plain-HTTP address never receives a token, even for the login's host.
func TestNoTokenOverPlainHTTP(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	f, err := New(Config{Source: modelrepo.SourceHF, Repo: "gated/model",
		Candidates: []proto.ModelFetchCandidate{{Base: server.URL}},
		Logins:     []modelrepo.Login{{Host: host, Secret: "hf_secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.List(context.Background(), commit); err == nil {
		t.Fatal("expected the unauthorized listing to fail")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, header := range sent {
		if header != "" {
			t.Fatalf("a plain-HTTP address was sent %q", header)
		}
	}
}
