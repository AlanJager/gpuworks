package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The branding wire: anyone (the sign-in page) reads it, only an admin writes
// it, a patch changes only the fields it names, an empty string restores the
// default, and the generic settings routes neither list nor accept it.
func TestBrandingAPI(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	call := func(token, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	const empty = `{"product_name":"","console_title":"","login_description":"","logo":""}`
	logo := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))

	if code, body := call("", http.MethodGet, "/api/v1/branding", ""); code != http.StatusOK || body != empty {
		t.Fatalf("anonymous default read = %d %s, want %s", code, body, empty)
	}
	set := `{"product_name":"  Acme AI  ","console_title":"Acme 算力平台","login_description":"第一行\n第二行","logo":"` + logo + `"}`
	if code, _ := call("", http.MethodPatch, "/api/v1/branding", set); code != http.StatusUnauthorized {
		t.Fatalf("anonymous write = %d, want 401", code)
	}
	if code, body := call(member, http.MethodPatch, "/api/v1/branding", set); code != http.StatusForbidden || !strings.Contains(body, ErrorAdministratorRequired) {
		t.Fatalf("member write = %d %s, want 403", code, body)
	}
	if st.Branding() != (store.Branding{}) {
		t.Fatal("a refused write changed the branding")
	}
	full := `{"product_name":"Acme AI","console_title":"Acme 算力平台","login_description":"第一行\n第二行","logo":"` + logo + `"}`
	if code, body := call(admin, http.MethodPatch, "/api/v1/branding", set); code != http.StatusOK || body != full {
		t.Fatalf("admin write = %d %s, want %s", code, body, full)
	}
	if code, body := call("", http.MethodGet, "/api/v1/branding", ""); code != http.StatusOK || body != full {
		t.Fatalf("anonymous read after write = %d %s", code, body)
	}

	bigLogo := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, brandingLogoMaxBytes+1))
	for name, bad := range map[string]string{
		"not json":           `not json`,
		"wrong type":         `{"product_name":7}`,
		"name too long":      `{"product_name":"` + strings.Repeat("名", brandingProductNameMax+1) + `"}`,
		"newline in title":   `{"console_title":"a\nb"}`,
		"control in blurb":   `{"login_description":"a\u0007b"}`,
		"logo not data url":  `{"logo":"https://example.com/logo.png"}`,
		"logo not base64":    `{"logo":"data:image/png,abc"}`,
		"logo html":          `{"logo":"data:text/html;base64,PGI+"}`,
		"logo bad base64":    `{"logo":"data:image/png;base64,***"}`,
		"logo empty payload": `{"logo":"data:image/png;base64,"}`,
		"logo too large":     `{"logo":"` + bigLogo + `"}`,
	} {
		code, body := call(admin, http.MethodPatch, "/api/v1/branding", bad)
		if code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", name, code, body)
		}
		if name != "not json" && name != "wrong type" && !strings.Contains(body, `"code":"FIELD_INVALID"`) {
			t.Fatalf("%s body = %s, want FIELD_INVALID", name, body)
		}
	}
	if code, body := call("", http.MethodGet, "/api/v1/branding", ""); body != full {
		t.Fatalf("refused patches changed the branding: %d %s", code, body)
	}

	// A patch naming one field keeps the others; "" restores that default.
	partial := `{"product_name":"Acme AI","console_title":"","login_description":"第一行\n第二行","logo":"` + logo + `"}`
	if code, body := call(admin, http.MethodPatch, "/api/v1/branding", `{"console_title":""}`); code != http.StatusOK || body != partial {
		t.Fatalf("partial patch = %d %s, want %s", code, body, partial)
	}

	if code, body := call(member, http.MethodGet, "/api/v1/settings", ""); code != http.StatusOK || strings.Contains(body, `"branding"`) {
		t.Fatalf("settings listing = %d %s, want no branding", code, body)
	}
	if code, body := call(admin, http.MethodPut, "/api/v1/settings/branding", `{"product_name":"x"}`); code != http.StatusBadRequest || !strings.Contains(body, `"code":"`+ErrorSettingHasOwnRoute+`"`) {
		t.Fatalf("generic write = %d %s, want 400 %s", code, body, ErrorSettingHasOwnRoute)
	}

	if code, body := call(admin, http.MethodPatch, "/api/v1/branding", `{"product_name":"","login_description":"","logo":""}`); code != http.StatusOK || body != empty {
		t.Fatalf("reset = %d %s, want %s", code, body, empty)
	}
}
