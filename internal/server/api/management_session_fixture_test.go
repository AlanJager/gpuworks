package api

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func managementSessionTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+filepath.Join(t.TempDir(), "auth")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Management authentication fixtures use the same persisted sessions and
// current user roles that the real router checks. Different roles get distinct
// users rather than relying on JWT claims to override a stored user's role.
func managementSessionTestToken(t *testing.T, st *store.Store, signer *auth.TokenSigner, claims auth.Claims) (string, error) {
	t.Helper()
	var user store.User
	err := st.DB().Where("username = ?", claims.Username).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		hash, hashErr := auth.HashPassword("password")
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		user = store.User{Username: claims.Username, Role: claims.Role, PasswordHash: hash}
		if err := st.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if user.Role != claims.Role {
		t.Fatalf("fixture user %q role = %q, want %q", user.Username, user.Role, claims.Role)
	}
	_, session, err := st.CreateManagementSession(&user, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claims.UserID = user.ID
	claims.SessionID = session.TokenHash
	return signer.Sign(claims)
}
