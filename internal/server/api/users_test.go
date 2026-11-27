package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// disabledAccountUser inserts an ordinary account and a login session for it.
func disabledAccountUser(t *testing.T, rig *tokenRig, username string) (store.User, string) {
	t.Helper()
	admin, err := rig.st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	user := store.User{Username: username, PasswordHash: admin.PasswordHash, Role: "user"}
	if err := rig.st.DB().Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: user.ID, Username: user.Username, Role: user.Role})
	if err != nil {
		t.Fatal(err)
	}
	return user, session
}

func TestAccountRoutesRequireAnAdministratorSession(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	admin, err := rig.st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	_, member := disabledAccountUser(t, rig, "member")
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodPost, fmt.Sprintf("/api/v1/users/%d/disable", admin.ID)},
		{http.MethodPost, fmt.Sprintf("/api/v1/users/%d/enable", admin.ID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/users/%d/identities", admin.ID)},
		{http.MethodPost, fmt.Sprintf("/api/v1/users/%d/identities", admin.ID)},
		{http.MethodDelete, fmt.Sprintf("/api/v1/users/%d/identities/1", admin.ID)},
	} {
		if rec := rig.do("", tc.method, tc.path, "{}"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d, want 401 (%s)", tc.method, tc.path, rec.Code, rec.Body)
		}
		rec := rig.do(member, tc.method, tc.path, "{}")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), ErrorAdministratorRequired) {
			t.Fatalf("member %s %s = %d, want 403 %s (%s)", tc.method, tc.path, rec.Code, ErrorAdministratorRequired, rec.Body)
		}
	}
	// A platform token cannot manage accounts either: the routes are session
	// only, like platform tokens.
	write := rig.mint("write")
	if rec := rig.do(write, http.MethodGet, "/api/v1/users", ""); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), ErrorPlatformTokenNotAllowed) {
		t.Fatalf("platform token GET /users = %d (%s), want 403 %s", rec.Code, rec.Body, ErrorPlatformTokenNotAllowed)
	}
}

func TestDisablingAnAccountEndsItsAccessAndEnablingRestoresIt(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	user, memberSession := disabledAccountUser(t, rig, "member")

	var list struct {
		Items []dto.User `json:"items"`
		Total int        `json:"total"`
	}
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/users", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || list.Total != 2 {
		t.Fatalf("list users = %d %s", rec.Code, rec.Body)
	}

	rec = rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/disable", user.ID), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	// The member's live session is over at once.
	if rec := rig.do(memberSession, http.MethodGet, "/api/v1/auth/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled session = %d (%s), want 401", rec.Code, rec.Body)
	}
	// And so is password sign-in, with the code that says why.
	rec = rig.do("", http.MethodPost, "/api/v1/auth/login", `{"username":"member","password":"gpuworks-test-password-2026"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), ErrorUserDisabled) {
		t.Fatalf("disabled login = %d %s, want 403 %s", rec.Code, rec.Body, ErrorUserDisabled)
	}

	rec = rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/enable", user.ID), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"disabled":false`) {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body)
	}
	rec = rig.do("", http.MethodPost, "/api/v1/auth/login", `{"username":"member","password":"gpuworks-test-password-2026"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled login = %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestTheLastLocalAdministratorCannotBeDisabled(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	admin, err := rig.st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	rec := rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/disable", admin.ID), "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), ErrorLastLocalAdmin) {
		t.Fatalf("disable last local admin = %d %s, want 403 %s", rec.Code, rec.Body, ErrorLastLocalAdmin)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/users", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"username":"admin"`) || strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("the administrator changed state: %d %s", rec.Code, rec.Body)
	}
}

func TestExternalIdentityLinksAreManagedAndScopedToTheirAccount(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	user, _ := disabledAccountUser(t, rig, "member")
	other, _ := disabledAccountUser(t, rig, "other")

	path := fmt.Sprintf("/api/v1/users/%d/identities", user.ID)
	// A malformed identity is a validation failure, not a silent no-op.
	for _, body := range []string{
		`{"source":"acme-oidc","subject":"u-1"}`,
		`{"source":"acme_oidc","subject":""}`,
		`{"source":"acme_oidc","subject":"` + strings.Repeat("x", 257) + `"}`,
	} {
		if rec := rig.do(rig.session, http.MethodPost, path, body); rec.Code != http.StatusBadRequest ||
			!strings.Contains(rec.Body.String(), ErrorExternalIdentityInvalid) {
			t.Fatalf("link %s = %d %s, want 400 %s", body, rec.Code, rec.Body, ErrorExternalIdentityInvalid)
		}
	}

	rec := rig.do(rig.session, http.MethodPost, path, `{"source":"acme_oidc","subject":"u-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link = %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("link body = %s", rec.Body)
	}
	// The same pair again is a conflict, whoever it would belong to.
	if rec := rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/identities", other.ID),
		`{"source":"acme_oidc","subject":"u-1"}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), ErrorExternalIdentityTaken) {
		t.Fatalf("duplicate link = %d %s, want 409", rec.Code, rec.Body)
	}
	// Listed, with its own id, source and subject.
	rec = rig.do(rig.session, http.MethodGet, path, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"source":"acme_oidc"`) ||
		!strings.Contains(rec.Body.String(), `"subject":"u-1"`) {
		t.Fatalf("list identities = %d %s", rec.Code, rec.Body)
	}
	// Another account cannot take it away by its id.
	if rec := rig.do(rig.session, http.MethodDelete,
		fmt.Sprintf("/api/v1/users/%d/identities/%d", other.ID, created.ID), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unlink through another account = %d (%s), want 404", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodDelete, path+fmt.Sprintf("/%d", created.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unlink = %d %s, want 204", rec.Code, rec.Body)
	}
	rec = rig.do(rig.session, http.MethodGet, path, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("identities after unlink = %d %s", rec.Code, rec.Body)
	}
}

func TestAccountRoutesAnswer404And405(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, tc := range []struct{ method, path, code string }{
		{http.MethodPost, "/api/v1/users/999999/disable", ErrorUserNotFound},
		{http.MethodPost, "/api/v1/users/999999/enable", ErrorUserNotFound},
		{http.MethodGet, "/api/v1/users/999999/identities", ErrorUserNotFound},
		{http.MethodPost, "/api/v1/users/999999/identities", ErrorUserNotFound},
		// The unlink answers for the link it was asked about, which is the
		// same 404 for a missing account and a link of another one.
		{http.MethodDelete, "/api/v1/users/999999/identities/1", ErrorExternalIdentityNotFound},
		{http.MethodPost, "/api/v1/users/not-a-number/disable", ErrorUserNotFound},
		// A link id that is not a number names no link of a real account.
		{http.MethodDelete, "/api/v1/users/1/identities/not-a-number", ErrorExternalIdentityNotFound},
	} {
		rec := rig.do(rig.session, tc.method, tc.path, `{"source":"acme_oidc","subject":"u-1"}`)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), tc.code) {
			t.Fatalf("%s %s = %d %s, want 404 %s", tc.method, tc.path, rec.Code, rec.Body, tc.code)
		}
		// An id parameter, where a 404 carries one, is always a number.
		var body struct {
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if id, ok := body.Params["id"]; ok {
			if _, isNumber := id.(float64); !isNumber {
				t.Fatalf("%s %s: params.id = %#v, want a number or no id", tc.method, tc.path, id)
			}
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		if rec := rig.do(rig.session, method, "/api/v1/users", "{}"); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /users = %d, want 405", method, rec.Code)
		}
	}
}

// While the platform is locked, an administrator can still take access away:
// disable an account and remove one of its external identities. Enabling an
// account and adding a link give access, so they are refused like any change.
func TestALockedPlatformStillTakesAccessAway(t *testing.T) {
	limiter := newSwitchLimiter(featuregate.Full)
	rig := newTokenRig(t, func(d *Deps) { d.Extensions = ExtensionDeps{Platform: limiter} })
	t.Cleanup(func() { _ = rig.st.Close() })
	user, _ := disabledAccountUser(t, rig, "member")
	identities := fmt.Sprintf("/api/v1/users/%d/identities", user.ID)
	rec := rig.do(rig.session, http.MethodPost, identities, `{"source":"acme_oidc","subject":"u-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link before the lock = %d %s", rec.Code, rec.Body)
	}
	var link struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &link); err != nil || link.ID == 0 {
		t.Fatalf("link body = %s", rec.Body)
	}

	limiter.set(PlatformLimit{Access: featuregate.Locked})
	refused := func(method, path, body string) {
		t.Helper()
		rec := rig.do(rig.session, method, path, body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), messages.ErrorPlatformLocked) {
			t.Fatalf("locked %s %s = %d %s, want 403 %s", method, path, rec.Code, rec.Body, messages.ErrorPlatformLocked)
		}
	}
	// Taking access away is served.
	if rec := rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/disable", user.ID), ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("locked disable = %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodDelete, fmt.Sprintf("%s/%d", identities, link.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("locked unlink = %d %s, want 204", rec.Code, rec.Body)
	}
	// Giving access is refused.
	refused(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/enable", user.ID), "")
	refused(http.MethodPost, identities, `{"source":"acme_oidc","subject":"u-2"}`)
	// Reading stays served.
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/users", ""); rec.Code != http.StatusOK {
		t.Fatalf("locked list = %d %s, want 200", rec.Code, rec.Body)
	}
}

// The assistant reads the API as the asking administrator; the account list
// and external identity subjects stay in the console and never reach a model.
func TestTheAssistantNeverReadsAccountsOrIdentities(t *testing.T) {
	for _, pattern := range []string{"/api/v1/users", "/api/v1/users/{id}/identities"} {
		if aihelper.Readable(pattern) {
			t.Fatalf("the assistant may read %s", pattern)
		}
		if aihelper.DeniedRoutes()[pattern] == "" {
			t.Fatalf("%s is denied without a reason", pattern)
		}
	}
}

// fixAccountTimes pins every account's and link's times, so wire JSON can be
// compared byte for byte. It writes the columns straight, around the store's
// own clock.
func fixAccountTimes(t *testing.T, rig *tokenRig) {
	t.Helper()
	db := rig.st.DB()
	if err := db.Exec(`UPDATE users SET created_at = ?, updated_at = ?`, "2026-11-01 08:00:00", "2026-11-02 09:00:00").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE users SET disabled_at = ? WHERE disabled_at IS NOT NULL`, "2026-11-03 10:00:00").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE user_identities SET created_at = ?`, "2026-11-04 11:00:00").Error; err != nil {
		t.Fatal(err)
	}
}

// assertNoSecret fails when an account body carries the password hash under
// any spelling.
func assertNoSecret(t *testing.T, body string) {
	t.Helper()
	for _, key := range []string{"password_hash", "PasswordHash", "password", "argon2"} {
		if strings.Contains(body, key) {
			t.Fatalf("an account body carries %q: %s", key, body)
		}
	}
}

// The accounts' wire format, compared byte for byte: every DTO field, with
// null for an account that is not disabled and a link not yet signed in
// through, and never the password hash.
func TestAccountsWireJSON(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	user, _ := disabledAccountUser(t, rig, "member")
	fixAccountTimes(t, rig)

	rec := rig.do(rig.session, http.MethodGet, "/api/v1/users", "")
	want := `{"items":[` +
		`{"id":1,"username":"admin","role":"admin","local_admin":true,"disabled":false,"disabled_at":null,"created_at":"2026-11-01T08:00:00Z","updated_at":"2026-11-02T09:00:00Z"},` +
		`{"id":2,"username":"member","role":"user","local_admin":false,"disabled":false,"disabled_at":null,"created_at":"2026-11-01T08:00:00Z","updated_at":"2026-11-02T09:00:00Z"}` +
		`],"total":2}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != want {
		t.Fatalf("GET /users = %d\n got: %s\nwant: %s", rec.Code, got, want)
	}
	assertNoSecret(t, rec.Body.String())

	// Disable answers the account as stored; its disabled_at is the store's
	// clock, so it is pinned and read back through the list.
	rec = rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/disable", user.ID), "")
	var disabled map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &disabled) != nil {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	if len(disabled) != 8 || disabled["disabled"] != true || disabled["disabled_at"] == nil || disabled["local_admin"] != false {
		t.Fatalf("disable body = %s", rec.Body)
	}
	assertNoSecret(t, rec.Body.String())
	fixAccountTimes(t, rig)
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/users", "")
	wantDisabled := `{"id":2,"username":"member","role":"user","local_admin":false,"disabled":true,"disabled_at":"2026-11-03T10:00:00Z","created_at":"2026-11-01T08:00:00Z","updated_at":"2026-11-02T09:00:00Z"}`
	if !strings.Contains(rec.Body.String(), wantDisabled) {
		t.Fatalf("listed disabled account:\n got: %s\nwant item: %s", rec.Body, wantDisabled)
	}

	rec = rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/enable", user.ID), "")
	fixAccountTimes(t, rig)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body)
	}
	var enabled map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &enabled); err != nil || len(enabled) != 8 ||
		enabled["disabled"] != false || enabled["disabled_at"] != nil {
		t.Fatalf("enable body = %s", rec.Body)
	}
	assertNoSecret(t, rec.Body.String())

	// Link answers 201 with the link; created_at is the store's clock, so
	// the listing after pinning carries the literal form.
	rec = rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/identities", user.ID), `{"source":"acme_oidc","subject":"u-1"}`)
	var created map[string]any
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &created) != nil ||
		len(created) != 5 || created["source"] != "acme_oidc" || created["subject"] != "u-1" || created["last_sign_in_at"] != nil {
		t.Fatalf("link = %d %s", rec.Code, rec.Body)
	}
	fixAccountTimes(t, rig)
	rec = rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/users/%d/identities", user.ID), "")
	wantLinks := fmt.Sprintf(`{"items":[{"id":%v,"source":"acme_oidc","subject":"u-1","created_at":"2026-11-04T11:00:00Z","last_sign_in_at":null}],"total":1}`, created["id"])
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != wantLinks {
		t.Fatalf("GET identities = %d\n got: %s\nwant: %s", rec.Code, got, wantLinks)
	}

	// Round trip: what the list says decodes into the DTO and encodes back
	// to the same bytes.
	var round dto.List[dto.User]
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/users", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &round); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(round)
	if err != nil || string(again) != strings.TrimSpace(rec.Body.String()) {
		t.Fatalf("round trip:\n got: %s\nwant: %s", again, rec.Body)
	}
}

// auth.local_password: local_admin keeps password sign-in for the local
// administrators only. The policy is judged after the password, so a wrong
// password tells nobody which policy is in force; the default lets everyone
// in.
func TestLocalPasswordPolicyLimitsPasswordSignIn(t *testing.T) {
	signIn := func(t *testing.T, policy auth.SessionPolicy, username, password string) *httptest.ResponseRecorder {
		t.Helper()
		rig := newTokenRig(t)
		t.Cleanup(func() { _ = rig.st.Close() })
		disabledAccountUser(t, rig, "member")
		signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		h := Router(Deps{AuthSigner: signer, Store: rig.st}, policy)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	limited := auth.SessionPolicy{LocalPassword: auth.LocalPasswordLocalAdmin}
	for _, tc := range []struct {
		name               string
		policy             auth.SessionPolicy
		username, password string
		status             int
		code               string
	}{
		{"member, right password, local_admin", limited, "member", "gpuworks-test-password-2026", http.StatusForbidden, ErrorLocalPasswordDisabled},
		{"member, wrong password, local_admin", limited, "member", "not-the-password-2026", http.StatusUnauthorized, ErrorInvalidCredentials},
		{"local administrator, local_admin", limited, "admin", "gpuworks-test-password-2026", http.StatusOK, ""},
		{"member, default policy", auth.SessionPolicy{}, "member", "gpuworks-test-password-2026", http.StatusOK, ""},
		{"member, all", auth.SessionPolicy{LocalPassword: auth.LocalPasswordAll}, "member", "gpuworks-test-password-2026", http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := signIn(t, tc.policy, tc.username, tc.password)
			if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`)) {
				t.Fatalf("login = %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			if tc.status != http.StatusOK && strings.Contains(rec.Body.String(), `"token"`) {
				t.Fatalf("a refused login handed out a token: %s", rec.Body)
			}
		})
	}
}
