package store

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestAdminCreationRejectsWeakPasswordsWithoutCreatingUsers(t *testing.T) {
	for _, password := range []string{"123", "password", "admin123"} {
		t.Run(password, func(t *testing.T) {
			s := newTestStore(t)
			t.Cleanup(func() { _ = s.Close() })
			if _, _, _, err := s.EnsureAdmin("admin", password); err == nil {
				t.Errorf("weak initial password was accepted")
			}
			var count int64
			if err := s.DB().Model(&User{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("rejected password left users: count=%d err=%v", count, err)
			}
		})
	}
}

func TestRejectedPasswordChangePreservesPasswordAndSessions(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	const original = "correct horse battery staple"
	if _, _, _, err := s.EnsureAdmin("admin", original); err != nil {
		t.Fatal(err)
	}
	u, ok := s.VerifyUser("admin", original)
	if !ok {
		t.Fatal("initial password rejected")
	}
	_, session, err := s.CreateManagementSession(u, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"123", "password", "admin123"} {
		if err := s.SetUserPassword("admin", password); err == nil {
			t.Errorf("weak replacement password was accepted")
		}
		current, valid := s.VerifyUser("admin", original)
		if !valid || current.PasswordHash != u.PasswordHash {
			t.Errorf("rejected password change altered the saved password")
		}
		if _, _, err := s.ManagementSessionByHash(session.TokenHash); err != nil {
			t.Errorf("rejected password change revoked a session: %v", err)
		}
	}
}

func TestPasswordPolicyPreservesExistingWeakAccountLogin(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	hash, err := auth.HashPassword("123")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&User{Username: "admin", PasswordHash: hash, Role: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, created, err := s.EnsureAdmin("admin", "123"); err != nil || created {
		t.Fatalf("existing account was revalidated or recreated: created=%v err=%v", created, err)
	}
	if _, valid := s.VerifyUser("admin", "123"); !valid {
		t.Fatal("existing weak password can no longer log in")
	}
	if err := s.SetUserPassword("admin", "sunrise over the quiet mountain"); err != nil {
		t.Fatal(err)
	}
	if _, valid := s.VerifyUser("admin", "123"); valid {
		t.Fatal("old password still logs in after replacement")
	}
	if _, valid := s.VerifyUser("admin", "sunrise over the quiet mountain"); !valid {
		t.Fatal("replacement passphrase rejected")
	}
}
