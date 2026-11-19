package pluginsdk_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// routePlugin serves two routes, each behind RequirePrincipal with its own
// action, and echoes what the handler saw of its caller.
func routePlugin(t *testing.T) *plugintest.Harness {
	t.Helper()
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := pluginsdk.PrincipalFrom(r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user": p.Username, "action": p.RouteAction, "route": p.Route,
			"local_admin": p.LocalAdmin, "access": pluginsdk.FeatureAccessFrom(r).String(),
		})
	})
	mux := http.NewServeMux()
	mux.Handle("POST /r/jobs", pluginsdk.RequirePrincipal("acme_backup.jobs_create", echo))
	mux.Handle("GET /r/jobs/{id}", pluginsdk.RequirePrincipal("acme_backup.jobs_read", echo))
	cfg := baseConfig()
	cfg.Handler = mux
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	return h
}

func send(t *testing.T, h *plugintest.Harness, req *http.Request) (int, http.Header, []byte) {
	t.Helper()
	resp, err := h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// A request the harness signs as the server does reaches the handler,
// which reads the verified caller and the feature access.
func TestRequirePrincipalServesASignedRequest(t *testing.T) {
	h := routePlugin(t)
	req := mustRequest(t, http.MethodPost, "/r/jobs")
	h.Sign(req, h.Principal("acme_backup.jobs_create", "/jobs"), extapi.FeatureAccess{"acme.backup": extapi.AccessReadOnly})
	status, _, body := send(t, h, req)
	want := `{"access":"acme.backup=read_only","action":"acme_backup.jobs_create","local_admin":true,"route":"/jobs","user":"admin"}` + "\n"
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("%d %s, want 200 %s", status, body, want)
	}
}

// Every request RequirePrincipal does not trust is refused with 403 and a
// product error the server can pass on, and never reaches the handler.
func TestRequirePrincipalRefuses(t *testing.T) {
	h := routePlugin(t)
	access := extapi.FeatureAccess{"acme.backup": extapi.AccessFull}
	cases := []struct {
		name string
		req  func() *http.Request
		code string
		want extapi.Params
	}{
		{"no principal", func() *http.Request { return mustRequest(t, http.MethodPost, "/r/jobs") },
			extapi.ErrorWireInvalid, extapi.Params{"field": "_root", "rule": extapi.RuleRequired}},
		{"action of another route", func() *http.Request {
			// The server matched the read route; the plugin's router
			// matched this path to the create route.
			r := mustRequest(t, http.MethodPost, "/r/jobs")
			h.Sign(r, h.Principal("acme_backup.jobs_read", "/jobs/{id}"), access)
			return r
		}, extapi.ErrorWireInvalid, extapi.Params{"field": "route_action", "rule": extapi.RuleRelation}},
		{"wrong key", func() *http.Request {
			r := mustRequest(t, http.MethodPost, "/r/jobs")
			hdr, mac, err := extapi.SignPrincipal(bytes.Repeat([]byte{8}, extapi.PrincipalKeyBytes),
				h.Principal("acme_backup.jobs_create", "/jobs"), access.String())
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set(extapi.HeaderPrincipal, hdr)
			r.Header.Set(extapi.HeaderPrincipalMac, mac)
			r.Header.Set(extapi.HeaderFeatureAccess, access.String())
			return r
		}, extapi.ErrorWireInvalid, extapi.Params{"field": "_root", "rule": extapi.RuleRelation}},
		{"feature access raised after signing", func() *http.Request {
			r := mustRequest(t, http.MethodPost, "/r/jobs")
			h.Sign(r, h.Principal("acme_backup.jobs_create", "/jobs"), extapi.FeatureAccess{"acme.backup": extapi.AccessReadOnly})
			r.Header.Set(extapi.HeaderFeatureAccess, access.String())
			return r
		}, extapi.ErrorWireInvalid, extapi.Params{"field": "_root", "rule": extapi.RuleRelation}},
		{"unknown field under a valid MAC", func() *http.Request {
			return signedRaw(t, h, `{"delegation":"x","exp":"2099-01-01T00:00:00Z","kind":"public","request_id":"r","route":"/jobs","route_action":"acme_backup.jobs_create"}`, access)
		}, extapi.ErrorWireUnknownField, extapi.Params{"field": "delegation"}},
		{"not canonical under a valid MAC", func() *http.Request {
			p := h.Principal("acme_backup.jobs_create", "/jobs")
			raw, err := json.MarshalIndent(p, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			return signedRaw(t, h, string(raw), access)
		}, extapi.ErrorWireNotCanonical, nil},
		{"longer than any principal", func() *http.Request {
			return signedRaw(t, h, strings.Repeat(" ", extapi.MaxPrincipalBytes+1), access)
		}, extapi.ErrorWireMalformed, extapi.Params{"rule": extapi.RuleRange}},
		{"expiry beyond any route's timeout", func() *http.Request {
			r := mustRequest(t, http.MethodPost, "/r/jobs")
			p := h.Principal("acme_backup.jobs_create", "/jobs")
			p.Exp = extapi.NewTime(time.Now().Add(time.Hour))
			h.Sign(r, p, access)
			return r
		}, extapi.ErrorWireInvalid, extapi.Params{"field": "exp", "rule": extapi.RuleRange}},
		{"expired", func() *http.Request {
			r := mustRequest(t, http.MethodPost, "/r/jobs")
			p := h.Principal("acme_backup.jobs_create", "/jobs")
			p.Exp = extapi.NewTime(time.Now().Add(-2 * time.Second))
			h.Sign(r, p, access)
			return r
		}, extapi.ErrorWireInvalid, extapi.Params{"field": "exp", "rule": extapi.RuleRange}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, hdr, body := send(t, h, c.req())
			if status != http.StatusForbidden || hdr.Get(extapi.HeaderProductError) != "1" {
				t.Fatalf("%d %v %s, want 403 with %s", status, hdr, body, extapi.HeaderProductError)
			}
			w, err := extapi.DecodeWireError(body)
			if err != nil {
				t.Fatal(err)
			}
			if w.Code != c.code || w.Reason != extapi.ReasonPermissionDenied || w.Params["object"] != "principal" {
				t.Fatalf("refusal %+v, want %s permission_denied on the principal", w, c.code)
			}
			for k, v := range c.want {
				if w.Params[k] != v {
					t.Fatalf("refusal params %v, want %s=%v", w.Params, k, v)
				}
			}
		})
	}
}

// Outside a request RequirePrincipal let through, there is no caller.
func TestPrincipalFromWithoutRequirePrincipal(t *testing.T) {
	r := mustRequest(t, http.MethodGet, "/r/jobs")
	if pluginsdk.PrincipalFrom(r) != nil || pluginsdk.FeatureAccessFrom(r) != nil {
		t.Fatal("a caller on a request RequirePrincipal never saw")
	}
}

// A handler mounted outside the plugin's Config.Handler has no key to
// verify with, so it refuses rather than serve unverified.
func TestRequirePrincipalOutsideThePluginRefuses(t *testing.T) {
	served := false
	h := pluginsdk.RequirePrincipal("acme_backup.jobs_create", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, mustRequest(t, http.MethodPost, "/r/jobs"))
	if served || w.Code != http.StatusInternalServerError {
		t.Fatalf("served %v, status %d", served, w.Code)
	}
}

// signedRaw is a request whose principal header is raw, as given, under a
// MAC the harness key makes valid: what a server bug would send.
func signedRaw(t *testing.T, h *plugintest.Harness, raw string, access extapi.FeatureAccess) *http.Request {
	t.Helper()
	r := mustRequest(t, http.MethodPost, "/r/jobs")
	hdr := base64.RawURLEncoding.EncodeToString([]byte(raw))
	m := hmac.New(sha256.New, h.PrincipalKey())
	m.Write([]byte(hdr + "\n" + access.String()))
	r.Header.Set(extapi.HeaderPrincipal, hdr)
	r.Header.Set(extapi.HeaderPrincipalMac, base64.RawURLEncoding.EncodeToString(m.Sum(nil)))
	r.Header.Set(extapi.HeaderFeatureAccess, access.String())
	return r
}

// A public caller, a platform token's caller and a route with no feature
// access all reach the handler as signed.
func TestRequirePrincipalServesEveryKind(t *testing.T) {
	h := routePlugin(t)
	public := &extapi.Principal{Kind: extapi.PrincipalPublic, RequestID: "r1",
		RouteAction: "acme_backup.jobs_create", Route: "/jobs", Exp: extapi.PrincipalExpiry(time.Now(), 0)}
	token := h.Principal("acme_backup.jobs_create", "/jobs")
	token.Kind, token.LocalAdmin, token.PlatformAdmin, token.Username = extapi.PrincipalToken, false, false, "member"
	token.Roles, token.APIKeyID = []string{"member"}, 9
	cases := []struct {
		name   string
		p      *extapi.Principal
		access extapi.FeatureAccess
		want   string
	}{
		{"public", public, extapi.FeatureAccess{"acme.backup": extapi.AccessLocked},
			`{"access":"acme.backup=locked","action":"acme_backup.jobs_create","local_admin":false,"route":"/jobs","user":""}`},
		{"token without feature access", token, nil,
			`{"access":"","action":"acme_backup.jobs_create","local_admin":false,"route":"/jobs","user":"member"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mustRequest(t, http.MethodPost, "/r/jobs")
			h.Sign(req, c.p, c.access)
			status, _, body := send(t, h, req)
			if status != http.StatusOK || string(body) != c.want+"\n" {
				t.Fatalf("%d %s, want 200 %s", status, body, c.want)
			}
		})
	}
}
