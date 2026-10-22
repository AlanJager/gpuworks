package app

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCommunityBrowseUsesTheManagementProxy(t *testing.T) {
	var upstream *httptest.Server
	var tokens atomic.Int32
	upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokens.Add(1)
			_, _ = io.WriteString(w, `{"token":"fixture-token"}`)
		case "/v2/repo/tags/list":
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+strings.Replace(upstream.URL, "127.0.0.1", "8.8.8.8", 1)+`/token",service="registry",scope="repository:repo:pull"`)
				w.WriteHeader(401)
				return
			}
			_, _ = io.WriteString(w, `{"tags":["v1.0.0"]}`)
		case "/tags":
			_, _ = io.WriteString(w, `{"next":"`+strings.Replace(upstream.URL, "127.0.0.1", "8.8.8.8", 1)+`/page2","results":[{"name":"v1.0.0"}]}`)
		case "/page2":
			_, _ = io.WriteString(w, `{"next":null,"results":[{"name":"v2.0.0"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	certFile, keyFile, roots := tlsTestFiles(t)
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	defer upstream.Close()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
	proxy := func(count *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				t.Errorf("proxy method = %s", r.Method)
				http.Error(w, "CONNECT required", 400)
				return
			}
			count.Add(1)
			// Loopback is never proxied, so the registry is named
			// 8.8.8.8 (the test certificate covers it); the proxy knows
			// where that is.
			dst, err := net.Dial("tcp", strings.Replace(r.Host, "8.8.8.8", "127.0.0.1", 1))
			if err != nil {
				http.Error(w, "upstream unavailable", 502)
				return
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				dst.Close()
				return
			}
			_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = buf.Flush()
			go func() { defer conn.Close(); defer dst.Close(); _, _ = io.Copy(dst, buf) }()
			_, _ = io.Copy(conn, dst)
			_ = conn.Close()
			_ = dst.Close()
		}))
	}
	var first, second atomic.Int32
	p1, p2 := proxy(&first), proxy(&second)
	defer p1.Close()
	defer p2.Close()
	application, _ := newTestApp(t, "password")
	t.Cleanup(func() { _ = application.Store().Close() })
	login := httptest.NewRecorder()
	application.http.Handler.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"password"}`)))
	if login.Code != 200 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	var credential struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	registry := strings.Replace(upstream.URL, "127.0.0.1", "8.8.8.8", 1)
	repos := `{"repos":[{"repo":"example/proxy-one","engine":"vllm","registry":"docker-hub","upstream_api":"` + registry + `/tags"},{"repo":"example/proxy-two","engine":"vllm","registry":"docker-hub","upstream_api":"` + registry + `/tags"},{"repo":"example/proxy-auth","engine":"vllm","registry":"oci","upstream_api":"` + registry + `/v2/repo/tags/list"}]}`
	if err := application.store.SetSetting(store.SettingImageCommunityCatalog, repos); err != nil {
		t.Fatal(err)
	}
	for i, p := range []*httptest.Server{p1, p2, p2} {
		existing, err := application.store.PullProxies()
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range existing {
			if err := application.store.DeletePullProxy(entry.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := application.store.CreatePullProxy(store.PullProxySpec{HTTPSProxy: p.URL, ControlPlane: true}, ""); err != nil {
			t.Fatal(err)
		}
		repo := []string{"example/proxy-one", "example/proxy-two", "example/proxy-auth"}[i]
		req := httptest.NewRequest(http.MethodGet, "/api/v1/images/community/browse?repo="+repo, nil)
		req.Header.Set("Authorization", "Bearer "+credential.Token)
		rec := httptest.NewRecorder()
		application.http.Handler.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "v1.0.0") {
			t.Fatalf("browse: %d %s", rec.Code, rec.Body.String())
		}
		if i < 2 && !strings.Contains(rec.Body.String(), "v2.0.0") {
			t.Fatal("pagination did not return second-page tags")
		}
		if i == 2 && tokens.Load() != 1 {
			t.Fatalf("anonymous token requests=%d", tokens.Load())
		}
		if []*atomic.Int32{&first, &second, &second}[i].Load() == 0 {
			t.Fatalf("management proxy %d received no registry traffic", i+1)
		}
	}
}
