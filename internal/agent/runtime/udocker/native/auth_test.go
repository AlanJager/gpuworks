package native

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// authRegistry fronts a fakeRegistry with the login flows real registries use:
// a bearer token fetched from a token endpoint with basic credentials, or
// basic credentials on every request.
type authRegistry struct {
	front     *httptest.Server
	mu        sync.Mutex
	tokenAuth []string // Authorization header of each token request
}

func newAuthRegistry(t *testing.T, reg *fakeRegistry, basicChallenge bool, user, secret string) *authRegistry {
	t.Helper()
	a := &authRegistry{}
	inner := reg.server.Config.Handler
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
	mux := http.NewServeMux()
	a.front = httptest.NewTLSServer(mux)
	t.Cleanup(a.front.Close)
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.tokenAuth = append(a.tokenAuth, r.Header.Get("Authorization"))
		a.mu.Unlock()
		if r.Header.Get("Authorization") != wantBasic {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"granted","expires_in":300}`))
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		ok := got == "Bearer granted"
		challenge := `Bearer realm="` + a.front.URL + `/token",service="fake"`
		if basicChallenge {
			ok = got == wantBasic
			challenge = `Basic realm="fake"`
		}
		if !ok {
			w.Header().Set("Www-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	return a
}

func (a *authRegistry) host() string { return strings.TrimPrefix(a.front.URL, "https://") }

func pullWithLogin(t *testing.T, reg *fakeRegistry, front *authRegistry, login *proto.SourceAuth) error {
	t.Helper()
	engine, _ := engineFor(t, reg, 0)
	engine.Client.HTTP = front.front.Client()
	ctx := context.Background()
	if login != nil {
		login.Host = front.host()
		ctx = runtime.WithRegistryAuth(ctx, []proto.SourceAuth{*login})
	}
	return engine.Pull(ctx, front.host()+"/"+reg.repo+":latest", nil)
}

func TestNativePullSendsLoginToTheTokenEndpoint(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	front := newAuthRegistry(t, reg, false, "robot", "tok3n")
	if err := pullWithLogin(t, reg, front, &proto.SourceAuth{Username: "robot", Secret: "tok3n"}); err != nil {
		t.Fatalf("authenticated pull: %v", err)
	}
	front.mu.Lock()
	defer front.mu.Unlock()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("robot:tok3n"))
	if len(front.tokenAuth) == 0 || front.tokenAuth[0] != want {
		t.Fatalf("token endpoint saw %v, want basic credentials", front.tokenAuth)
	}
}

func TestNativePullAnswersBasicChallengeWithLogin(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	front := newAuthRegistry(t, reg, true, "robot", "tok3n")
	if err := pullWithLogin(t, reg, front, &proto.SourceAuth{Username: "robot", Secret: "tok3n"}); err != nil {
		t.Fatalf("basic-auth pull: %v", err)
	}
}

func TestNativePullWithoutOrWithWrongLoginFailsWithoutLeakingTheSecret(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	for name, login := range map[string]*proto.SourceAuth{
		"anonymous": nil,
		"wrong":     {Username: "robot", Secret: "wrong-s3cret"},
	} {
		front := newAuthRegistry(t, reg, false, "robot", "tok3n")
		err := pullWithLogin(t, reg, front, login)
		if err == nil {
			t.Fatalf("%s: pull of a protected image succeeded", name)
		}
		if strings.Contains(err.Error(), "wrong-s3cret") || strings.Contains(err.Error(), "tok3n") {
			t.Fatalf("%s: error leaks a secret: %v", name, err)
		}
	}
}

func TestNativePullIgnoresLoginForOtherRegistries(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	front := newAuthRegistry(t, reg, false, "robot", "tok3n")
	engine, _ := engineFor(t, reg, 0)
	engine.Client.HTTP = front.front.Client()
	ctx := runtime.WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "elsewhere.example", Username: "robot", Secret: "tok3n"}})
	if err := engine.Pull(ctx, front.host()+"/"+reg.repo+":latest", nil); err == nil {
		t.Fatal("a login for another registry authenticated this pull")
	}
	front.mu.Lock()
	defer front.mu.Unlock()
	for _, h := range front.tokenAuth {
		if h != "" {
			t.Fatalf("credentials sent to the wrong registry: %q", h)
		}
	}
}
