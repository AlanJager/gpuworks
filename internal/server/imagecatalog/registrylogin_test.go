package imagecatalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRegistry answers like Docker Hub or Harbor: /v2/ challenges for a bearer
// token, and the token service accepts only the right basic credentials.
func fakeRegistry(t *testing.T, user, pass string, basicOnly bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		good := ok && u == user && p == pass
		switch r.URL.Path {
		case "/v2/":
			if basicOnly {
				if good {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/service/token",service="harbor-registry"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/service/token":
			if r.URL.Query().Get("service") != "harbor-registry" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !good {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"token":"t"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckRegistryLoginBearerAndBasic(t *testing.T) {
	for _, basic := range []bool{false, true} {
		srv := fakeRegistry(t, "ci", "s3cret", basic)
		host := strings.TrimPrefix(srv.URL, "https://")
		if err := CheckRegistryLogin(context.Background(), srv.Client(), host, "ci", "s3cret"); err != nil {
			t.Fatalf("basic=%v good login: %v", basic, err)
		}
		if err := CheckRegistryLogin(context.Background(), srv.Client(), host, "ci", "wrong"); !errors.Is(err, ErrRegistryLoginRejected) {
			t.Fatalf("basic=%v bad login err = %v, want rejected", basic, err)
		}
	}
}

func TestCheckRegistryLoginReportsUnreachableAndOddAnswers(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate", `Bearer service="x"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	err := CheckRegistryLogin(context.Background(), srv.Client(), host, "u", "p")
	if err == nil || errors.Is(err, ErrRegistryLoginRejected) || !strings.Contains(err.Error(), "without saying where") {
		t.Fatalf("challenge without realm err = %v", err)
	}
	srv.Close()
	if err := CheckRegistryLogin(context.Background(), srv.Client(), host, "u", "p"); err == nil || errors.Is(err, ErrRegistryLoginRejected) {
		t.Fatalf("unreachable registry err = %v", err)
	}
	if got := registryAPIHost("docker.io"); got != "registry-1.docker.io" {
		t.Fatalf("docker hub api host = %q", got)
	}
}
