package modelmeta

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// hfFixture answers the Hugging Face model API and resolve endpoints for
// acme/model; missing is a mirror that lacks the repository.
func hfFixture(t *testing.T, missing bool, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if missing {
			http.Error(w, `{"error":"Repository not found"}`, http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case "/api/models/acme/model":
			_, _ = w.Write([]byte(`{"author":"acme","createdAt":"2025-01-02T03:04:05Z"}`))
		case "/api/organizations/acme/avatar":
			_, _ = w.Write([]byte(`{"avatarUrl":"http://` + r.Host + `/uploads/acme.png"}`))
		case "/acme/model/resolve/main/config.json":
			_, _ = w.Write([]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`))
		case "/acme/model/resolve/main/model.safetensors.index.json":
			_, _ = w.Write([]byte(`{"metadata":{"total_size":1234}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func deadAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	_ = listener.Close()
	return address
}

// The first address that serves the repository answers the whole fetch: one
// that refuses connections or lacks the repository is passed over, and the
// address that answered is reported.
func TestFetchFromReadsTheFirstAddressThatServesTheRepository(t *testing.T) {
	var missingRequests, liveRequests atomic.Int32
	missing := hfFixture(t, true, &missingRequests)
	live := hfFixture(t, false, &liveRequests)
	meta, err := FetchFrom(context.Background(), "huggingface", "acme/model", "main", "", []string{deadAddress(t), missing.URL, live.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Endpoint != live.URL || meta.Arch != "Qwen3ForCausalLM" || meta.WeightBytes != 1234 || meta.LogoURL != live.URL+"/uploads/acme.png" {
		t.Fatalf("meta = %+v, want everything from %s", meta, live.URL)
	}
	if missingRequests.Load() != 2 {
		t.Fatalf("the mirror without the repository was asked %d times, want only the probe (its record, then config.json)", missingRequests.Load())
	}
}

// No address answering is not an error - a registration goes ahead without
// metadata - but the empty Endpoint says nothing was read.
func TestFetchFromWithNoAddressAnsweringReadsNothing(t *testing.T) {
	var requests atomic.Int32
	missing := hfFixture(t, true, &requests)
	meta, err := FetchFrom(context.Background(), "huggingface", "acme/model", "main", "", []string{deadAddress(t), missing.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Endpoint != "" || meta.Config != nil || meta.WeightBytes != 0 {
		t.Fatalf("meta = %+v, want nothing read", meta)
	}
	if _, err := FetchFrom(context.Background(), "huggingface", "acme/model", "main", "", nil, nil, nil); err == nil {
		t.Fatal("no address at all is an error")
	}
}

// A mirror that proxies only the files - no repository record - still serves
// the fetch when the address before it is dead.
func TestFetchFromAcceptsAMirrorWithoutTheRepositoryRecord(t *testing.T) {
	var requests atomic.Int32
	filesOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/acme/model/resolve/main/config.json":
			_, _ = w.Write([]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer filesOnly.Close()
	meta, err := FetchFrom(context.Background(), "huggingface", "acme/model", "main", "", []string{deadAddress(t), filesOnly.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Endpoint != filesOnly.URL || meta.Arch != "Qwen3ForCausalLM" || meta.LogoURL != "" {
		t.Fatalf("meta = %+v", meta)
	}
}

// The management node's reads carry a stored login as a node's downloads do:
// to its own host over https, never over plain HTTP.
func TestFetchFromCarriesTheLoginForItsHostOverHTTPSOnly(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer hf_secret" {
				http.Error(w, `{"error":"gated"}`, http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/api/models/gated/model":
				_, _ = w.Write([]byte(`{"author":"gated"}`))
			case "/gated/model/resolve/main/config.json":
				_, _ = w.Write([]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`))
			default:
				http.NotFound(w, r)
			}
		})
	}
	secure := httptest.NewTLSServer(handler("https"))
	defer secure.Close()
	plain := httptest.NewServer(handler("http"))
	defer plain.Close()
	logins := []modelrepo.Login{
		{Host: strings.TrimPrefix(secure.URL, "https://"), Secret: "hf_secret"},
		{Host: strings.TrimPrefix(plain.URL, "http://"), Secret: "hf_secret"},
	}
	meta, err := FetchFrom(context.Background(), "huggingface", "gated/model", "main", "", []string{plain.URL, secure.URL}, secure.Client(), logins)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Endpoint != secure.URL || meta.Arch != "Qwen3ForCausalLM" {
		t.Fatalf("meta = %+v", meta)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, header := range seen["http"] {
		if header != "" {
			t.Fatalf("a plain-HTTP address was sent %q", header)
		}
	}
	if len(seen["https"]) == 0 || seen["https"][0] != "Bearer hf_secret" {
		t.Fatalf("the https host was sent %v", seen["https"])
	}
}

// gatedMirror answers as a mirror without the login answers a gated
// repository: its record is public, its files are refused (hf-mirror says 403).
func gatedMirror(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/acme/model" {
			_, _ = w.Write([]byte(`{"author":"acme","gated":"manual"}`))
			return
		}
		if r.URL.Path == "/api/organizations/acme/avatar" {
			_, _ = w.Write([]byte(`{"avatarUrl":"http://` + r.Host + `/uploads/acme.png"}`))
			return
		}
		http.Error(w, "Access to model acme/model is restricted", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A mirror that shows a gated repository's record but refuses its files is
// passed over for the address that lets them in (the hub, with the login).
func TestFetchFromPassesOverAnAddressThatRefusesTheFiles(t *testing.T) {
	var requests atomic.Int32
	mirror := gatedMirror(t)
	hub := hfFixture(t, false, &requests)
	meta, err := FetchFrom(context.Background(), "huggingface", "acme/model", "main", "", []string{mirror.URL, hub.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Endpoint != hub.URL || meta.Arch != "Qwen3ForCausalLM" {
		t.Fatalf("meta = %+v, want the files read from %s", meta, hub.URL)
	}
}

// When every address refuses the files, the first that answered is read: its
// record is still worth keeping.
func TestFetchFromReadsTheFirstRefusingAddressWhenAllRefuse(t *testing.T) {
	first, second := gatedMirror(t), gatedMirror(t)
	meta, err := FetchFrom(context.Background(), "huggingface", "acme/model", "main", "", []string{deadAddress(t), first.URL, second.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Endpoint != first.URL || meta.Config != nil || meta.LogoURL != first.URL+"/uploads/acme.png" {
		t.Fatalf("meta = %+v, want the record of %s and no files", meta, first.URL)
	}
}
