package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// aiConfigRig is a router over an in-memory store with a real secretbox, so a
// key is encrypted the way production encrypts it, plus a token to call it.
type aiConfigRig struct {
	router http.Handler
	store  *store.Store
	box    *auth.Secretbox
	signer *auth.TokenSigner
	token  string
}

func newAIConfigRig(t *testing.T, allowPrivateHosts ...string) *aiConfigRig {
	t.Helper()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	signer, err := auth.NewTokenSigner("ai-config-edit", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("master")
	if err != nil {
		t.Fatal(err)
	}
	rig := &aiConfigRig{store: st, box: box, signer: signer, token: token}
	rig.router = rig.routerAllowing(append([]string{"127.0.0.1"}, allowPrivateHosts...))
	return rig
}

// routerAllowing rebuilds the routes with one outbound allowlist, so a test can
// show what an address the policy refuses looks like.
func (r *aiConfigRig) routerAllowing(allowPrivateHosts []string) http.Handler {
	return Router(Deps{
		Store: r.store, AuthSigner: r.signer, AISecretbox: r.box,
		Outside: OutsideSources{AllowPrivateHosts: allowPrivateHosts},
	})
}

func (r *aiConfigRig) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.token)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

// create stores one service through the API and answers its id.
func (r *aiConfigRig) create(t *testing.T, body string) uint {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/api/v1/ai/configs", body)
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); rec.Code != http.StatusCreated || err != nil || created.ID == 0 {
		t.Fatalf("create = %d %s (err %v)", rec.Code, rec.Body.String(), err)
	}
	return created.ID
}

// config reads one row as the list reports it.
func (r *aiConfigRig) config(t *testing.T, id uint) dtoRow {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/api/v1/ai/configs", "")
	var rows []dtoRow
	if err := unmarshalList(rec.Body.Bytes(), &rows); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("list = %d %s (err %v)", rec.Code, rec.Body.String(), err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("config %d missing from %+v", id, rows)
	return dtoRow{}
}

// dtoRow is one wire row: exactly the fields the console shows.
type dtoRow struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	Preset    string `json:"preset"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	IsDefault bool   `json:"is_default"`
}

// storedKey answers the key as persisted, so a test can tell a kept key from a
// rewritten one.
func (r *aiConfigRig) storedKey(t *testing.T, id uint) string {
	t.Helper()
	c, err := r.store.AIConfigByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return c.APIKey
}

// The edit dialog sends only the fields it changed; the rest must survive, and
// a blank key must keep the stored one.
func TestUpdateAIConfigAppliesEachFieldAlone(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","preset":"deepseek","base_url":"https://api.deepseek.com","api_key":"sk-original","model":"deepseek-v4-flash","is_default":true}`)
	before := rig.config(t, id)
	keyBefore := rig.storedKey(t, id)
	if keyBefore == "sk-original" || !strings.HasPrefix(keyBefore, "enc:v1:") {
		t.Fatalf("key was not encrypted: %q", keyBefore)
	}
	for _, tc := range []struct {
		name  string
		body  string
		check func(t *testing.T, row dtoRow)
	}{
		{"name", `{"name":"renamed"}`, func(t *testing.T, row dtoRow) {
			if row.Name != "renamed" {
				t.Fatalf("name = %q", row.Name)
			}
		}},
		{"preset", `{"preset":"custom"}`, func(t *testing.T, row dtoRow) {
			if row.Preset != "custom" {
				t.Fatalf("preset = %q", row.Preset)
			}
		}},
		{"base_url", `{"base_url":"https://api.deepseek.com/v1"}`, func(t *testing.T, row dtoRow) {
			if row.BaseURL != "https://api.deepseek.com/v1" {
				t.Fatalf("base_url = %q", row.BaseURL)
			}
		}},
		{"model", `{"model":"deepseek-v4"}`, func(t *testing.T, row dtoRow) {
			if row.Model != "deepseek-v4" {
				t.Fatalf("model = %q", row.Model)
			}
		}},
		{"is_default", `{"is_default":false}`, func(t *testing.T, row dtoRow) {
			if row.IsDefault {
				t.Fatal("is_default not cleared")
			}
		}},
	} {
		rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: patch = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		tc.check(t, rig.config(t, id))
		// Every other field is untouched, and the key is never returned.
		after := rig.config(t, id)
		if after.ID != before.ID || after.Model == "" || after.Name == "" || after.BaseURL == "" {
			t.Fatalf("%s: other fields changed: %+v -> %+v", tc.name, before, after)
		}
		if strings.Contains(rec.Body.String(), "api_key") || strings.Contains(rec.Body.String(), "sk-original") {
			t.Fatalf("%s: the response carried a key: %s", tc.name, rec.Body.String())
		}
		if got := rig.storedKey(t, id); got != keyBefore {
			t.Fatalf("%s: the stored key changed: %q -> %q", tc.name, keyBefore, got)
		}
	}
}

// A blank key in the patch keeps the stored ciphertext byte for byte; a filled
// one replaces it with a fresh encryption of the new key.
func TestUpdateAIConfigKeyBlankKeepsStoredFilledReplaces(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","base_url":"https://api.deepseek.com/v1","api_key":"sk-original","model":"m"}`)
	kept := rig.storedKey(t, id)
	if rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"api_key":""}`); rec.Code != http.StatusOK {
		t.Fatalf("blank key patch = %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.storedKey(t, id); got != kept {
		t.Fatalf("a blank key rewrote the stored one: %q -> %q", kept, got)
	}
	if rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"api_key":"sk-replaced"}`); rec.Code != http.StatusOK {
		t.Fatalf("filled key patch = %d %s", rec.Code, rec.Body.String())
	}
	replaced := rig.storedKey(t, id)
	if replaced == kept {
		t.Fatal("a filled key did not replace the stored one")
	}
	plain, err := store.DecryptAIKey(rig.box, replaced)
	if err != nil || plain != "sk-replaced" {
		t.Fatalf("stored key decrypts to %q (%v)", plain, err)
	}
}

// A config stored before keys were encrypted keeps its plaintext value when an
// edit does not name the key: the dialog never saw the key, so it cannot send
// it back, and re-encrypting would need the operator to retype it.
func TestUpdateAIConfigKeepsLegacyPlaintextKey(t *testing.T) {
	rig := newAIConfigRig(t)
	c, err := rig.store.CreateAIConfig(&store.AIConfig{Name: "legacy", BaseURL: "https://api.deepseek.com", APIKey: "sk-plaintext", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", c.ID), `{"name":"renamed","api_key":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.storedKey(t, c.ID); got != "sk-plaintext" {
		t.Fatalf("legacy key changed to %q", got)
	}
}

// Marking a service the default clears the mark on the others, so the console
// and the assistant agree on which one is in use.
func TestUpdateAIConfigKeepsOneDefault(t *testing.T) {
	rig := newAIConfigRig(t)
	first := rig.create(t, `{"name":"first","base_url":"https://api.deepseek.com","model":"m"}`)
	second := rig.create(t, `{"name":"second","base_url":"https://api.deepseek.com","model":"m"}`)
	if !rig.config(t, first).IsDefault {
		t.Fatal("the first service should be the default")
	}
	rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", second), `{"is_default":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	if rig.config(t, first).IsDefault || !rig.config(t, second).IsDefault {
		t.Fatalf("two defaults: first=%+v second=%+v", rig.config(t, first), rig.config(t, second))
	}
}

func TestUpdateAIConfigUnknownID(t *testing.T) {
	rig := newAIConfigRig(t)
	rec := rig.do(t, http.MethodPatch, "/api/v1/ai/configs/404", `{"name":"gone"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch of a missing service = %d %s", rec.Code, rec.Body.String())
	}
}

// A rejected value must name the field, so the dialog marks the input the
// operator has to fix instead of showing a banner.
func TestUpdateAIConfigRejectsInvalidValues(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","base_url":"https://api.deepseek.com","model":"m"}`)
	path := fmt.Sprintf("/api/v1/ai/configs/%d", id)
	long := strings.Repeat("a", 65)
	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"a blank name", `{"name":"   "}`, "name"},
		{"a name with control characters", `{"name":"bad\u0000name"}`, "name"},
		{"a model longer than its column", fmt.Sprintf(`{"model":%q}`, long), "model"},
		{"a blank model", `{"model":"  "}`, "model"},
		{"a blank address", `{"base_url":"   "}`, "base_url"},
		{"an address without a scheme", `{"base_url":"api.deepseek.com"}`, "base_url"},
		{"an address of another scheme", `{"base_url":"file:///etc/passwd"}`, "base_url"},
		{"an address that is only the call path", `{"base_url":"/chat/completions"}`, "base_url"},
		{"a malformed body", `{"name":`, "body"},
	} {
		rec := rig.do(t, http.MethodPatch, path, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: patch = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		var answer struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: %v in %s", tc.name, err, rec.Body.String())
		}
		if answer.Code != ErrorFieldInvalid || answer.Params["field"] != tc.field {
			t.Fatalf("%s: code=%s params=%v", tc.name, answer.Code, answer.Params)
		}
	}
	// None of the rejections changed the stored service.
	row := rig.config(t, id)
	if row.Name != "primary" || row.BaseURL != "https://api.deepseek.com" || row.Model != "m" {
		t.Fatalf("a rejected patch changed the service: %+v", row)
	}
}

// The edit route is a management write: it needs a token and refuses the wrong
// method rather than answering a router default.
func TestAIConfigEditRoutesRequireAuthentication(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","base_url":"https://api.deepseek.com","model":"m"}`)
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"name":"x"}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		rig.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d", tc.method, tc.path, rec.Code)
		}
	}
	if rec := rig.do(t, http.MethodPut, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"name":"x"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on a service = %d", rec.Code)
	}
	if rec := rig.do(t, http.MethodPatch, "/api/v1/ai/configs", `{"name":"x"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH on the collection = %d", rec.Code)
	}
}
