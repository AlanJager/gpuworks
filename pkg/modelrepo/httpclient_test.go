package modelrepo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectoryClientKeepsARequestOnTheRegisteredHost(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "weights from an unrelated host")
	}))
	defer elsewhere.Close()

	var mirror *httptest.Server
	mirror = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/offsite"):
			http.Redirect(w, r, elsewhere.URL+"/model.safetensors", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/onsite"):
			http.Redirect(w, r, mirror.URL+"/final", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/credentials"):
			http.Redirect(w, r, "http://user:secret@"+r.Host+"/final", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/scheme"):
			w.Header().Set("Location", "ftp://"+r.Host+"/final")
			w.WriteHeader(http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/loop"):
			http.Redirect(w, r, mirror.URL+"/loop", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "the real weights")
		}
	}))
	defer mirror.Close()

	client, err := DirectoryClient(mirror.URL+"/Qwen/Qwen3-0.6B/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, wantErr string
	}{
		{name: "same host is followed", path: "/onsite"},
		{name: "another host is refused", path: "/offsite", wantErr: "redirected from host"},
		{name: "credentials are refused", path: "/credentials", wantErr: "embedding credentials"},
		{name: "another scheme is refused", path: "/scheme", wantErr: "unsupported scheme"},
		{name: "an endless chain is cut", path: "/loop", wantErr: "more than 5 redirects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.Get(mirror.URL + tc.path)
			if resp != nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if tc.wantErr == "" && string(body) != "the real weights" {
					t.Fatalf("body = %q", body)
				}
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("same-host redirect failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestDirectoryClientKeepsTheBaseClientsTransport(t *testing.T) {
	proxied := false
	base := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		proxied = true
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	client, err := DirectoryClient("https://mirror.example.com/Qwen/Qwen3-0.6B", base)
	if err != nil {
		t.Fatal(err)
	}
	if client == base {
		t.Fatal("the caller's client was mutated instead of copied")
	}
	if base.CheckRedirect != nil {
		t.Fatal("the caller's client gained a redirect policy")
	}
	resp, err := client.Get("https://mirror.example.com/Qwen/Qwen3-0.6B/config.json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !proxied {
		t.Fatal("the node's transport was not used")
	}
}

func TestDirectoryClientRefusesAURLItCannotConfine(t *testing.T) {
	for _, raw := range []string{"", "ftp://mirror.example.com/models", "https://user:secret@mirror.example.com/models", "https:///models"} {
		if _, err := DirectoryClient(raw, nil); err == nil {
			t.Fatalf("DirectoryClient(%q) was accepted", raw)
		}
	}
}

func TestHeadFileReportsTheExactLengthAndType(t *testing.T) {
	body := strings.Repeat("w", 1503300328%9973)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model.safetensors":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("Accept-Ranges", "bytes")
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, body)
			}
		case "/index":
			w.Header().Set("Content-Type", "text/html;charset=UTF-8")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	size, contentType, err := HeadFile(context.Background(), server.URL+"/model.safetensors", server.Client())
	if err != nil || size != int64(len(body)) || contentType != "application/octet-stream" {
		t.Fatalf("head = %d %q %v, want %d application/octet-stream", size, contentType, err, len(body))
	}
	// A directory answers HEAD too; the content type is what tells the two
	// apart, so it must survive the parameters a server appends.
	if _, contentType, err := HeadFile(context.Background(), server.URL+"/index", server.Client()); err != nil || contentType != "text/html" {
		t.Fatalf("directory head = %q %v, want text/html", contentType, err)
	}
	if _, _, err := HeadFile(context.Background(), server.URL+"/gone", server.Client()); err == nil || !strings.Contains(err.Error(), "http 404") {
		t.Fatalf("missing file head error = %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
