//go:build linux && dqlite

package app

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// memberAdminSession signs a management session for a fresh administrator
// on st. The API accepts a bearer token only for a stored session.
func memberAdminSession(t *testing.T, st *store.Store, secret string) (*auth.TokenSigner, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	user, err := st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := st.CreateManagementSession(user, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(auth.Claims{UserID: user.ID, Username: user.Username, Role: user.Role, SessionID: session.TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	return signer, token
}

// fencedBusinessWrite is a business write the store admits only through
// its business write barrier. It changes nothing: no model has this id.
func fencedBusinessWrite(st *store.Store) error {
	return st.FillModelWeight(1<<30, 1)
}
