package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
)

// handSigned is a session token for userID signed with the rig's secret
// over a payload the test writes, the way only the server could sign one.
func handSigned(userID uint, sid, extra string) string {
	enc := base64.RawURLEncoding.EncodeToString
	body := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		enc([]byte(fmt.Sprintf(`{"sub":%d,"username":"ops","role":"admin","sid":%q,"exp":%d%s}`, userID, sid, time.Now().Add(time.Hour).Unix(), extra)))
	m := hmac.New(sha256.New, []byte("platform-token-test-secret"))
	m.Write([]byte(body))
	return body + "." + enc(m.Sum(nil))
}

func meWire(t *testing.T, rig *tokenRig, bearer string) map[string]any {
	t.Helper()
	rec := rig.do(bearer, http.MethodGet, "/api/v1/auth/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// /auth/me reports the effective break-glass flag: the stored flag of an
// administrator signed in with a login session. A platform token, an
// administrator without the flag, a token that claims it and a demoted
// administrator all read false.
func TestMeReportsTheEffectiveLocalAdministratorFlag(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	want := map[string]any{}
	if err := json.Unmarshal([]byte(`{"id":1,"username":"admin","role":"admin","local_admin":true,"password_policy":{"min_length":8,"max_length":128}}`), &want); err != nil {
		t.Fatal(err)
	}
	if got := meWire(t, rig, rig.session); !reflect.DeepEqual(got, want) {
		t.Fatalf("me of the bootstrapped administrator = %v, want %v", got, want)
	}
	for _, scope := range []string{"read", "write"} {
		if got := meWire(t, rig, rig.mint(scope)); got["local_admin"] != false || got["username"] != "admin" {
			t.Fatalf("me with the local administrator's %s token = %v; a platform token never carries the flag", scope, got)
		}
	}

	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{Username: "ops", Role: "admin", LocalAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := meWire(t, rig, ops); got["local_admin"] != false || got["role"] != "admin" {
		t.Fatalf("me of an administrator created later = %v; signing a claim does not grant the flag", got)
	}
	opsClaims, err := signer.Verify(ops)
	if err != nil {
		t.Fatal(err)
	}
	forged := handSigned(opsClaims.UserID, opsClaims.SessionID, `,"local_admin":true`)
	if got := meWire(t, rig, forged); got["local_admin"] != false {
		t.Fatalf("me with a token claiming local_admin = %v", got)
	}

	if err := rig.st.DB().Exec(`UPDATE users SET role = 'user' WHERE username = 'admin'`).Error; err != nil {
		t.Fatal(err)
	}
	if got := meWire(t, rig, rig.session); got["local_admin"] != false || got["role"] != "user" {
		t.Fatalf("me of a demoted administrator = %v; the flag must stop counting without a write", got)
	}
	var stored bool
	if err := rig.st.DB().Raw(`SELECT local_admin FROM users WHERE username = 'admin'`).Scan(&stored).Error; err != nil || !stored {
		t.Fatalf("stored flag after the demotion = %v, %v; the demotion itself should not write it", stored, err)
	}
}

// countingSource answers Locked for every feature and counts the questions.
type countingSource struct{ calls atomic.Int64 }

func (c *countingSource) Availability(featuregate.Key) featuregate.Availability {
	c.calls.Add(1)
	return featuregate.Availability{Access: featuregate.Locked}
}

// countingBanners counts the manifests that asked plugins for banners.
type countingBanners struct{ calls atomic.Int64 }

func (c *countingBanners) Banners(_ context.Context, _ auth.Claims) []dto.ConsoleBanner {
	c.calls.Add(1)
	return nil
}

// The recovery surface (sign in, who am I, change my password, plugin
// administration) passes no feature gate and asks nothing of plugins: with
// every feature locked and every plugin-fed source counting, each answers
// and none is consulted. A gated extension route and the console manifest
// over the same sources first show the counters see a consultation.
func TestRecoveryRoutesNeverConsultTheFeatureGateOrPlugins(t *testing.T) {
	src, banners, plugins := &countingSource{}, &countingBanners{}, &fakePlugins{}
	rig := newTokenRig(t, func(d *Deps) {
		d.Extensions.Features = mustGate(t, src, featuregate.Feature{Key: "acme.backup", Presentation: featuregate.Promote})
		d.Extensions.Banners = []BannerSource{banners}
		d.Extensions.Plugins = plugins
		d.Extensions.Extensions = []Extension{fakeExtension{name: "acme", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/acme/backups", Feature: "acme.backup"}}}}
	})
	t.Cleanup(func() { _ = rig.st.Close() })
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/x/acme/backups", ""); rec.Code != http.StatusForbidden || src.calls.Load() == 0 {
		t.Fatalf("a gated extension route = %d %s after %d gate questions; the counters must see a consultation", rec.Code, rec.Body, src.calls.Load())
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/console/manifest", ""); rec.Code != http.StatusOK || banners.calls.Load() == 0 {
		t.Fatalf("the console manifest = %d %s after %d banner questions; the counters must see a consultation", rec.Code, rec.Body, banners.calls.Load())
	}
	src.calls.Store(0)
	banners.calls.Store(0)

	rec := rig.do("", http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"gpuworks-test-password-2026"}`)
	var login struct {
		Token string `json:"token"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &login) != nil || login.Token == "" {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	for _, step := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/auth/me", ""},
		{http.MethodGet, "/api/v1/plugins", ""},
		{http.MethodPost, "/api/v1/plugins/install?approve=sha256:00", "the package bytes"},
		{http.MethodPost, "/api/v1/plugins/acme_backup/disable", ""},
		{http.MethodPost, "/api/v1/plugins/acme_backup/enable", ""},
		{http.MethodDelete, "/api/v1/plugins/acme_backup", ""},
		{http.MethodPost, "/api/v1/auth/password", `{"current_password":"gpuworks-test-password-2026","new_password":"recovered-password-2026"}`},
	} {
		if rec := rig.do(login.Token, step.method, step.path, step.body); rec.Code < 200 || rec.Code > 299 {
			t.Fatalf("%s %s = %d %s", step.method, step.path, rec.Code, rec.Body)
		}
	}
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("the recovery surface asked the feature gate %d times", n)
	}
	if n := banners.calls.Load(); n != 0 {
		t.Fatalf("the recovery surface asked plugins for banners %d times", n)
	}
	if got := strings.Join(plugins.recorded(), "|"); got != "list|install sha256:00 keep admin|disable acme_backup admin|enable acme_backup admin|uninstall acme_backup keep admin" {
		t.Fatalf("plugin administration calls = %s", got)
	}
}
