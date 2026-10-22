package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"encoding/pem"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// Registry credentials never leave the server: no response carries the secret,
// only has_secret. Writes re-sync online nodes (they hold the logins from
// their last desired state) and nobody else.
func TestRegistryCredentialsWire(t *testing.T) {
	rig := newPullRig(t)
	rig.node("online-1", true, nil)
	rig.node("offline-1", false, nil)
	rig.node("online-2", true, nil)
	rig.want("GET", "/api/v1/pull/credentials", "", 200, `{"items":[],"total":0}`)

	// The registry is normalized to the host; username is trimmed.
	code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials", `{"host":" https://GHCR.io/ ","username":" bot ","secret":"s3cret-token"}`)
	jsonEqual(t, json.RawMessage(body), `{"id":1,"host":"ghcr.io","username":"bot","has_secret":true}`)
	if code != 201 || strings.Contains(body, "s3cret") {
		t.Fatalf("create = %d %s", code, body)
	}
	if got := rig.syncedNodes(); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"online-1", "online-2"}) {
		t.Fatalf("synced after create = %v, want exactly the online nodes", got)
	}
	_, body = rig.do(rig.admin, "GET", "/api/v1/pull/credentials", "")
	jsonEqual(t, json.RawMessage(body), `{"total":1,"items":[{"id":1,"host":"ghcr.io","username":"bot","has_secret":true}]}`)
	if strings.Contains(body, "s3cret") {
		t.Fatalf("list leaks the secret: %s", body)
	}

	// An update without a secret keeps the stored one.
	code, body = rig.do(rig.admin, "PUT", "/api/v1/pull/credentials/1", `{"host":"ghcr.io","username":"renamed"}`)
	jsonEqual(t, json.RawMessage(body), `{"id":1,"host":"ghcr.io","username":"renamed","has_secret":true}`)
	if code != 200 {
		t.Fatalf("update = %d %s", code, body)
	}
	auths, err := rig.st.SourceAuths()
	if err != nil || len(auths) != 1 || auths[0].Username != "renamed" || auths[0].Secret != "s3cret-token" {
		t.Fatalf("stored login after an update without a secret = %+v err=%v", auths, err)
	}
	// An update with one replaces it.
	if code, body = rig.do(rig.admin, "PUT", "/api/v1/pull/credentials/1", `{"host":"ghcr.io","username":"renamed","secret":"second"}`); code != 200 || strings.Contains(body, "second") {
		t.Fatalf("update with secret = %d %s", code, body)
	}
	if auths, _ := rig.st.SourceAuths(); len(auths) != 1 || auths[0].Secret != "second" {
		t.Fatalf("stored login after a new secret = %+v", auths)
	}
	// The secret is encrypted at rest, never the plain text.
	var cipher string
	if err := rig.st.DB().Raw("SELECT secret_cipher FROM source_credentials WHERE id = 1").Scan(&cipher).Error; err != nil || cipher == "" || strings.Contains(cipher, "second") {
		t.Fatalf("stored cipher = %q err=%v", cipher, err)
	}

	before := len(rig.syncedNodes())
	rig.want("DELETE", "/api/v1/pull/credentials/1", "", 204, "")
	if got := rig.syncedNodes(); len(got) != before+2 {
		t.Fatalf("delete synced %v, want the two online nodes told", got[before:])
	}
	rig.want("GET", "/api/v1/pull/credentials", "", 200, `{"items":[],"total":0}`)
}

func TestRegistryCredentialValidationAndMissingRows(t *testing.T) {
	rig := newPullRig(t)
	rig.node("online", true, nil)
	if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials", `{"host":"ghcr.io","username":"bot","secret":"x"}`); code != 201 {
		t.Fatalf("seed = %d %s", code, body)
	}
	syncs := len(rig.syncedNodes())
	for name, tc := range map[string]struct{ method, path, body, field string }{
		"a host that is not a host":  {"POST", "/api/v1/pull/credentials", `{"host":"not a host","username":"u","secret":"s"}`, "host"},
		"a host with a path":         {"POST", "/api/v1/pull/credentials", `{"host":"ghcr.io/org","username":"u","secret":"s"}`, "host"},
		"no host":                    {"POST", "/api/v1/pull/credentials", `{"host":"","username":"u","secret":"s"}`, "host"},
		"a new login needs a secret": {"POST", "/api/v1/pull/credentials", `{"host":"quay.io","username":"u"}`, "secret"},
		"one login per host":         {"POST", "/api/v1/pull/credentials", `{"host":"GHCR.io","username":"u","secret":"s"}`, "host"},
	} {
		code, body := rig.do(rig.admin, tc.method, tc.path, tc.body)
		var got struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: %v %s", name, err, body)
		}
		if code != 400 || got.Code != "PULL_SETTING_INVALID" || got.Params["field"] != tc.field {
			t.Errorf("%s: %d %s, want 400 PULL_SETTING_INVALID on %s", name, code, body, tc.field)
		}
	}
	if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials", `{"host":"quay.io","username":"u","secret":"s","extra":1}`); code != 400 {
		t.Errorf("unknown field = %d %s", code, body)
	}
	if got := len(rig.syncedNodes()); got != syncs {
		t.Fatalf("refused writes synced nodes (%d -> %d)", syncs, got)
	}
	for _, route := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/pull/credentials/42", `{"host":"quay.io","username":"u","secret":"s"}`},
		{"PUT", "/api/v1/pull/credentials/0", `{"host":"quay.io","username":"u","secret":"s"}`},
		{"DELETE", "/api/v1/pull/credentials/42", ``},
		{"POST", "/api/v1/pull/credentials/42/test", ``},
	} {
		if code, body := rig.do(rig.admin, route.method, route.path, route.body); code != 404 {
			t.Errorf("%s %s = %d %s, want 404", route.method, route.path, code, body)
		}
	}
}

// A login test runs on the server through the server's own network settings;
// it reports what the server sees.
func TestRegistryCredentialTestErrorMapping(t *testing.T) {
	rig := newPullRig(t)
	create := func(registry string) {
		t.Helper()
		if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials", fmt.Sprintf(`{"host":%q,"username":"bot","secret":"s3cret"}`, registry)); code != 201 {
			t.Fatalf("create %s = %d %s", registry, code, body)
		}
	}

	// A registry nobody answers for is a bad gateway that names the registry
	// and carries the reason, and never the secret.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	_ = closed.Close()
	create(address)
	code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials/1/test", "")
	var failed struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &failed); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusBadGateway || failed.Code != "CREDENTIAL_LOGIN_FAILED" || failed.Params["host"] != address ||
		!strings.Contains(fmt.Sprint(failed.Params["detail"]), address) || strings.Contains(body, "s3cret") {
		t.Fatalf("unreachable registry = %d %s", code, body)
	}

	// The test goes out through the management node's proxy: with one chosen
	// the registry is reached by CONNECT through it.
	var mu sync.Mutex
	var connects []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connects = append(connects, r.Method+" "+r.Host)
		mu.Unlock()
		http.Error(w, "proxy says no", http.StatusForbidden)
	}))
	defer proxy.Close()
	if code, text := rig.do(rig.admin, "POST", "/api/v1/pull/proxies", fmt.Sprintf(`{"https_proxy":%q,"control_plane":true}`, proxy.URL)); code != 201 {
		t.Fatalf("create proxy = %d %s", code, text)
	}
	create("8.8.8.8")
	code, body = rig.do(rig.admin, "POST", "/api/v1/pull/credentials/2/test", "")
	if code != http.StatusBadGateway || !strings.Contains(body, `"code":"CREDENTIAL_LOGIN_FAILED"`) || !strings.Contains(body, `"host":"8.8.8.8"`) {
		t.Fatalf("proxied test = %d %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(connects, []string{"CONNECT 8.8.8.8:443"}) {
		t.Fatalf("the proxy saw %v, want one CONNECT to the registry", connects)
	}
}

// A login whose secret cannot be decrypted (the server's key changed) is a
// conflict, not a registry failure: the operator must enter it again.
func TestRegistryCredentialTestWithAnUnreadableSecret(t *testing.T) {
	rig := newPullRig(t)
	if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials", `{"host":"ghcr.io","username":"bot","secret":"s3cret"}`); code != 201 {
		t.Fatalf("create = %d %s", code, body)
	}
	other, err := auth.NewSecretbox("a-different-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	rig.st.SetSecretbox(other)
	code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials/1/test", "")
	var got struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusConflict || got.Code != "CREDENTIAL_LOGIN_FAILED" || got.Params["detail"] == "" || strings.Contains(body, "s3cret") {
		t.Fatalf("unreadable secret = %d %s", code, body)
	}
}

// The success and rejection paths need a registry the server trusts, which an
// httptest server's certificate is not. The scenario therefore runs in a child
// copy of this test binary whose system roots are the fake registry's
// certificate (SSL_CERT_FILE); the parent owns the registry and checks what it
// received.
const loginChildEnv = "GPUWORKS_TEST_REGISTRY_LOGIN_HOST"

type loginAttempt struct{ path, user, secret string }

func TestRegistryCredentialTestAgainstATrustedRegistry(t *testing.T) {
	var mu sync.Mutex
	var attempts []loginAttempt
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, secret, _ := r.BasicAuth()
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			secret = token
		}
		mu.Lock()
		attempts = append(attempts, loginAttempt{r.URL.Path, user, secret})
		mu.Unlock()
		switch r.URL.Path {
		case "/v2/":
			switch user {
			case "basic-ok":
				w.WriteHeader(http.StatusOK)
			case "basic-bad":
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(http.StatusUnauthorized)
			case "token-user":
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake-registry"`, server.URL))
				w.WriteHeader(http.StatusUnauthorized)
			default:
				http.Error(w, "boom", http.StatusInternalServerError)
			}
		case "/api/whoami-v2":
			// A Hugging Face style hub: who the token belongs to.
			if secret != "hf_good" {
				http.Error(w, `{"error":"Invalid credentials in Authorization header"}`, http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"type":"user","name":"bot"}`))
		case "/token":
			if r.URL.Query().Get("service") != "fake-registry" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if secret != "right-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"token":"t"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	certFile := filepath.Join(t.TempDir(), "registry.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistryLoginChild$", "-test.v")
	cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+certFile, "SSL_CERT_DIR="+t.TempDir(), loginChildEnv+"="+strings.TrimPrefix(server.URL, "https://"))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestRegistryLoginChild") {
		t.Fatalf("child run failed: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []loginAttempt{
		{"/v2/", "basic-ok", "pw-1"},
		{"/v2/", "basic-bad", "pw-2"},
		{"/v2/", "token-user", "wrong-token"},
		{"/token", "token-user", "wrong-token"},
		{"/v2/", "token-user", "right-token"},
		{"/token", "token-user", "right-token"},
		{"/v2/", "server-error", "pw-3"},
		{"/api/whoami-v2", "", "hf_bad"},
		{"/api/whoami-v2", "", "hf_good"},
	}
	if !reflect.DeepEqual(attempts, want) {
		t.Fatalf("the registry received %+v, want the decrypted login of each step: %+v", attempts, want)
	}
}

// TestRegistryLoginChild is the child half of the test above; on its own it
// has nothing to do.
func TestRegistryLoginChild(t *testing.T) {
	host := os.Getenv(loginChildEnv)
	if host == "" {
		t.Skip("runs only as the child of TestRegistryCredentialTestAgainstATrustedRegistry")
	}
	rig := newPullRig(t)
	if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials", fmt.Sprintf(`{"host":%q,"username":"basic-ok","secret":"pw-1"}`, host)); code != 201 {
		t.Fatalf("create = %d %s", code, body)
	}
	for _, step := range []struct {
		user, secret string
		status       int
		code         string
	}{
		{"basic-ok", "pw-1", 200, ""},
		{"basic-bad", "pw-2", http.StatusUnprocessableEntity, "CREDENTIAL_LOGIN_REJECTED"},
		{"token-user", "wrong-token", http.StatusUnprocessableEntity, "CREDENTIAL_LOGIN_REJECTED"},
		{"token-user", "right-token", 200, ""},
		{"server-error", "pw-3", http.StatusBadGateway, "CREDENTIAL_LOGIN_FAILED"},
		// No user name: a model site's token, tested against the hub's whoami.
		{"", "hf_bad", http.StatusUnprocessableEntity, "CREDENTIAL_LOGIN_REJECTED"},
		{"", "hf_good", 200, ""},
	} {
		if step.user != "basic-ok" {
			if code, body := rig.do(rig.admin, "PUT", "/api/v1/pull/credentials/1", fmt.Sprintf(`{"host":%q,"username":%q,"secret":%q}`, host, step.user, step.secret)); code != 200 {
				t.Fatalf("update %s = %d %s", step.user, code, body)
			}
		}
		code, body := rig.do(rig.admin, "POST", "/api/v1/pull/credentials/1/test", "")
		if code != step.status {
			t.Fatalf("%s: status %d %s, want %d", step.user, code, body, step.status)
		}
		if step.code == "" {
			jsonEqual(t, json.RawMessage(body), `{"ok":true}`)
			continue
		}
		var got struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Code != step.code || got.Params["host"] != host || strings.Contains(body, step.secret) {
			t.Fatalf("%s: %s, want %s for %s without the secret", step.user, body, step.code, host)
		}
		if step.code == "CREDENTIAL_LOGIN_FAILED" && !strings.Contains(fmt.Sprint(got.Params["detail"]), "answered 500") {
			t.Fatalf("%s: detail %v does not say what the registry answered", step.user, got.Params["detail"])
		}
		if step.code == "CREDENTIAL_LOGIN_REJECTED" && len(got.Params) != 1 {
			t.Fatalf("%s: rejected params %v, want only the registry", step.user, got.Params)
		}
	}
}

// Every route the registry logins had answers 410 with the new route.
func TestRegistryCredentialRoutesAreGone(t *testing.T) {
	rig := newPullRig(t)
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/v1/pull/registry-credentials", ``},
		{"POST", "/api/v1/pull/registry-credentials", `{"registry":"ghcr.io","username":"u","secret":"s"}`},
		{"PUT", "/api/v1/pull/registry-credentials/1", `{}`},
		{"DELETE", "/api/v1/pull/registry-credentials/1", ``},
		{"POST", "/api/v1/pull/registry-credentials/1/test", ``},
	} {
		code, body := rig.do(rig.admin, route.method, route.path, route.body)
		if code != http.StatusGone || !strings.Contains(body, `"code":"SETTING_REMOVED"`) || !strings.Contains(body, `"route":"/api/v1/pull/credentials"`) {
			t.Errorf("%s %s = %d %s", route.method, route.path, code, body)
		}
	}
	if creds, _ := rig.st.SourceCredentials(); len(creds) != 0 {
		t.Fatalf("a removed route stored a login: %+v", creds)
	}
}
