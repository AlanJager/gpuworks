package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInferenceKeyManagementUsesTheAuthenticatedOwner(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	admin, err := rig.st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	userA := store.User{Username: "key-owner-a", PasswordHash: admin.PasswordHash, Role: "user"}
	userB := store.User{Username: "key-owner-b", PasswordHash: admin.PasswordHash, Role: "user"}
	for _, user := range []*store.User{&userA, &userB} {
		if err := rig.st.DB().Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	login := func(user string) string {
		t.Helper()
		rec := rig.do("", http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"username":%q,"password":"gpuworks-test-password-2026"}`, user))
		var body struct {
			Token string `json:"token"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Token == "" {
			t.Fatalf("login=%d %s", rec.Code, rec.Body)
		}
		return body.Token
	}
	sessionA, sessionB := login(userA.Username), login(userB.Username)
	create := func(session, body string) string {
		t.Helper()
		rec := rig.do(session, http.MethodPost, "/api/v1/apikeys", body)
		var created struct {
			Key string `json:"key"`
		}
		if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Key == "" {
			t.Fatalf("create=%d %s", rec.Code, rec.Body)
		}
		return created.Key
	}
	keyA := create(sessionA, fmt.Sprintf(`{"name":"a","user_id":%d}`, userB.ID))
	keyB := create(sessionB, `{"name":"b"}`)
	for _, row := range []struct {
		key   string
		owner uint
	}{{keyA, userA.ID}, {keyB, userB.ID}} {
		identity, ok := rig.st.ResolveAPIKey(row.key)
		if !ok || identity.UserID != row.owner {
			t.Errorf("key owner=%d valid=%v, want %d from login", identity.UserID, ok, row.owner)
		}
	}
	legacy, err := rig.st.CreateAPIKey("legacy", 0)
	if err != nil {
		t.Fatal(err)
	}
	list := func(session string) []map[string]any {
		t.Helper()
		rec := rig.do(session, http.MethodGet, "/api/v1/apikeys", "")
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
			t.Fatalf("list=%d %s", rec.Code, rec.Body)
		}
		return page.Items
	}
	for _, row := range []struct {
		session, name string
		owner         uint
	}{{sessionA, "a", userA.ID}, {sessionB, "b", userB.ID}} {
		keys := list(row.session)
		if len(keys) != 1 || keys[0]["name"] != row.name || keys[0]["user_id"] != float64(row.owner) {
			t.Errorf("personal key list=%v, want only %s with owner %d", keys, row.name, row.owner)
		}
	}
	adminKeys := list(rig.session)
	if len(adminKeys) != 3 {
		t.Errorf("admin key count=%d, want 3", len(adminKeys))
	}
	foundLegacy := false
	for _, k := range adminKeys {
		if k["name"] == "legacy" {
			foundLegacy = true
			if k["user_id"] != float64(0) {
				t.Errorf("legacy ownership must explicitly remain zero: %v", k)
			}
		}
	}
	if !foundLegacy {
		t.Error("administrator cannot find the legacy key")
	}
	for _, target := range []string{auth.PrefixOf(keyB), auth.PrefixOf(legacy)} {
		for _, op := range []struct{ method, path, body string }{{http.MethodPut, "/scope", `{"all_deployments":true}`}, {http.MethodDelete, "", ""}} {
			rec := rig.do(sessionA, op.method, "/api/v1/apikeys/"+target+op.path, op.body)
			if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"API_KEY_NOT_FOUND"`) {
				t.Errorf("cross-owner operation=%d %s", rec.Code, rec.Body)
			}
		}
	}
	if _, ok := rig.st.ResolveAPIKey(keyB); !ok {
		t.Error("another user's revoke invalidated the key")
	}
	// An unavailable owner read must not become a successful mutation, even
	// when writes remain healthy. Keep the callback registered throughout;
	// toggling an atomic flag also keeps this fixture safe under -race.
	deployment, err := rig.st.CreateDeployment(&store.Deployment{Name: "scope-target", ServedName: "scope-target"})
	if err != nil {
		t.Fatal(err)
	}
	var failKeyRead atomic.Bool
	const hook = "test:inference-key-owner-read"
	if err := rig.st.DB().Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "api_keys" && failKeyRead.Load() {
			tx.AddError(errors.New("injected API key read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rig.st.DB().Callback().Query().Remove(hook) })
	for _, op := range []struct{ method, path, body string }{
		{http.MethodGet, "", ""},
		{http.MethodPut, "/" + auth.PrefixOf(keyA) + "/scope", fmt.Sprintf(`{"all_deployments":false,"deployment_ids":[%d]}`, deployment.ID)},
		{http.MethodDelete, "/" + auth.PrefixOf(keyA), ""},
	} {
		failKeyRead.Store(true)
		rec := rig.do(sessionA, op.method, "/api/v1/apikeys"+op.path, op.body)
		failKeyRead.Store(false)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("unavailable key read=%d %s", rec.Code, rec.Body)
		}
		identity, ok := rig.st.ResolveAPIKey(keyA)
		if !ok || identity.Restricted {
			t.Errorf("unavailable owner read changed key: valid=%v restricted=%v", ok, identity.Restricted)
		}
	}
	for _, session := range []string{sessionA, rig.session} {
		prefix := auth.PrefixOf(keyA)
		if session == rig.session {
			prefix = auth.PrefixOf(keyB)
		}
		rec := rig.do(session, http.MethodPut, "/api/v1/apikeys/"+prefix+"/scope", `{"all_deployments":true}`)
		if rec.Code != 200 {
			t.Errorf("owner/admin scope=%d %s", rec.Code, rec.Body)
		}
		rec = rig.do(session, http.MethodDelete, "/api/v1/apikeys/"+prefix, "")
		if rec.Code != 200 {
			t.Errorf("owner/admin revoke=%d %s", rec.Code, rec.Body)
		}
	}
	if _, ok := rig.st.ResolveAPIKey(keyA); ok {
		t.Error("owner-revoked key still authenticates")
	}
	if _, ok := rig.st.ResolveAPIKey(keyB); ok {
		t.Error("admin-revoked key still authenticates")
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/apikeys/"+auth.PrefixOf(legacy)+"/scope", `{"all_deployments":true}`); rec.Code != 200 {
		t.Errorf("administrator cannot scope legacy key=%d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/apikeys/"+auth.PrefixOf(legacy), ""); rec.Code != 200 {
		t.Errorf("administrator cannot revoke legacy key=%d %s", rec.Code, rec.Body)
	}
}
