package store

import (
	"crypto/sha256"
	"errors"
	"strconv"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// EnsureAdmin creates the admin user if none exists, returning (username,
// generatedPassword, created). When created is false the password is "".
func (s *Store) EnsureAdmin(username, password string) (string, string, bool, error) {
	var count int64
	if err := s.db.Model(&User{}).Count(&count).Error; err != nil {
		return "", "", false, err
	}
	if count > 0 {
		return username, "", false, nil
	}
	if password == "" {
		password = auth.NewToken(9)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", "", false, err
	}
	u := &User{Username: username, PasswordHash: hash, Role: "admin"}
	if err := s.db.Create(u).Error; err != nil {
		return "", "", false, err
	}
	return username, password, true, nil
}

// VerifyUser checks a username/password pair.
func (s *Store) VerifyUser(username, password string) (*User, bool) {
	u, err := s.AuthenticateUser(username, password)
	return u, err == nil
}

// AuthenticateUser distinguishes rejected credentials from storage failures.
func (s *Store) AuthenticateUser(username, password string) (*User, error) {
	var u User
	if err := s.db.Where("username = ?", username).First(&u).Error; err != nil {
		return nil, sessionLookupError(err)
	}
	if !auth.VerifyPassword(password, u.PasswordHash) {
		return nil, ErrNotFound
	}
	return &u, nil
}

// SetUserPassword replaces a user's password hash.
func (s *Store) SetUserPassword(username, password string) error {
	if password == "" {
		return errors.New("password must not be empty")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	var user User
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("username = ?", username).First(&user).Error; err != nil {
			return sessionLookupError(err)
		}
		if err := tx.Model(&user).Update("password_hash", hash).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", user.ID).Delete(&ManagementSession{}).Error
	})
	if err != nil {
		return err
	}
	s.publishManagementSession(strconv.FormatUint(uint64(user.ID), 10), "password-changed")
	return nil
}

// CreateAPIKey generates and stores a new API key, returning the full key once.
func (s *Store) CreateAPIKey(name string, userID uint) (fullKey string, err error) {
	full, prefix := auth.NewAPIKey()
	hash, err := auth.HashPassword(full)
	if err != nil {
		return "", err
	}
	k := &APIKey{Name: name, Prefix: prefix, Hash: hash, UserID: userID}
	if err := s.db.Create(k).Error; err != nil {
		return "", err
	}
	s.apiKeys.changed()
	s.Bus.PublishChange("apikey", prefix, "created")
	return full, nil
}

// ListAPIKeys returns key metadata (never the secret), revoked keys included.
func (s *Store) ListAPIKeys() ([]APIKey, error) {
	var ks []APIKey
	err := s.db.Order("id desc").Find(&ks).Error
	return ks, err
}

// RevokeAPIKey stops a key authenticating, keeping its row (and so its name
// in the usage it has). Revoking an unknown or already revoked key is a
// no-op.
func (s *Store) RevokeAPIKey(prefix string) error {
	result := s.db.Model(&APIKey{}).Where("prefix = ? AND revoked_at IS NULL", prefix).
		Update("revoked_at", time.Now().UTC())
	// Forget the key even when the write errored: re-verifying against the
	// database is always safe, trusting a possibly revoked key is not.
	s.apiKeys.forget(prefix)
	if result.Error == nil && result.RowsAffected > 0 {
		s.Bus.PublishChange("apikey", prefix, "revoked")
	}
	return result.Error
}

// APIKeyIdentity is who presented a valid API key: the key and its owner.
type APIKeyIdentity struct {
	KeyID  uint
	UserID uint
}

// VerifyAPIKey validates a presented key (prefix lookup + argon2id verify) and
// touches LastUsed.
func (s *Store) VerifyAPIKey(presented string) bool {
	_, ok := s.ResolveAPIKey(presented)
	return ok
}

// ResolveAPIKey is VerifyAPIKey that also says which key and owner the
// secret belongs to; the gateway attributes each request to them.
func (s *Store) ResolveAPIKey(presented string) (APIKeyIdentity, bool) {
	digest := sha256.Sum256([]byte(presented))
	if cached, ok := s.apiKeys.lookup(digest); ok {
		s.touchAPIKey(cached.id)
		return APIKeyIdentity{KeyID: cached.id, UserID: cached.userID}, true
	}
	call, leader := s.apiKeys.join(digest)
	if !leader {
		<-call.done
		if call.ok {
			s.touchAPIKey(call.id)
		}
		return APIKeyIdentity{KeyID: call.id, UserID: call.userID}, call.ok
	}
	defer s.apiKeys.finish(digest, call)
	generation := s.apiKeys.snapshot()
	var k APIKey
	if err := s.db.Where("prefix = ? AND revoked_at IS NULL", auth.PrefixOf(presented)).First(&k).Error; err != nil {
		// Not found and read errors both reject; neither is cached.
		return APIKeyIdentity{}, false
	}
	if !verifyAPIKeyHash(presented, k.Hash) {
		return APIKeyIdentity{}, false
	}
	call.id, call.userID, call.ok = k.ID, k.UserID, true
	s.apiKeys.remember(generation, digest, k.ID, k.UserID, k.Prefix)
	s.touchAPIKey(k.ID)
	return APIKeyIdentity{KeyID: k.ID, UserID: k.UserID}, true
}

func (s *Store) touchAPIKey(id uint) {
	if now, due := s.apiKeys.dueLastUsed(id); due {
		s.db.Model(&APIKey{}).Where("id = ?", id).Update("last_used", now)
	}
}

// HasAnyAPIKey reports whether at least one unrevoked API key exists. The
// answer is cached until a key is created or revoked through this store.
func (s *Store) HasAnyAPIKey() bool {
	some, known, generation := s.apiKeys.cachedHasAny()
	if known {
		return some
	}
	var count int64
	if err := s.db.Model(&APIKey{}).Where("revoked_at IS NULL").Count(&count).Error; err != nil {
		// A failed read is not cached, so it cannot pin the answer.
		return count > 0
	}
	s.apiKeys.setHasAny(generation, count > 0)
	return count > 0
}
