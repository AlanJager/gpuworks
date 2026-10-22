package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// CreateManagementSession creates a revocable credential after verifying that
// the password snapshot used at login still matches the account in storage.
func (s *Store) CreateManagementSession(user *User, idleTTL, maxTTL time.Duration) (string, *ManagementSession, error) {
	if user == nil {
		return "", nil, ErrNotFound
	}
	if idleTTL <= 0 || maxTTL <= 0 {
		return "", nil, errors.New("session lifetimes must be positive")
	}
	raw := auth.NewToken(32)
	if raw == "" {
		return "", nil, errors.New("could not generate management session credential")
	}
	var session ManagementSession
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var current User
		if err := tx.Where("id = ? AND password_hash = ?", user.ID, user.PasswordHash).First(&current).Error; err != nil {
			return sessionLookupError(err)
		}
		now := time.Now().UTC()
		if err := tx.Where("expires_at <= ? OR absolute_expires_at <= ?", now, now).Delete(&ManagementSession{}).Error; err != nil {
			return err
		}
		session = ManagementSession{TokenHash: HashToken(raw), UserID: current.ID, CreatedAt: now, ExpiresAt: now.Add(idleTTL), AbsoluteExpiresAt: now.Add(maxTTL)}
		if session.ExpiresAt.After(session.AbsoluteExpiresAt) {
			session.ExpiresAt = session.AbsoluteExpiresAt
		}
		return tx.Create(&session).Error
	})
	if err != nil {
		return "", nil, err
	}
	return raw, &session, nil
}

// ManagementSessionByHash reads the account and session in one database
// snapshot. Roles and revocations are always read from authoritative storage.
//
// It runs on every authenticated management request, so it reads through the
// read pool: on the writer's single connection each request queued behind
// whatever status-update write was in flight (see Store.readDB). WAL readers
// see every committed revocation, so nothing is served stale.
func (s *Store) ManagementSessionByHash(hash string) (*User, *ManagementSession, error) {
	var user *User
	var session *ManagementSession
	err := s.reader().Transaction(func(tx *gorm.DB) error {
		var err error
		user, session, err = managementSessionByHash(tx, hash, time.Now().UTC())
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return user, session, nil
}

func managementSessionByHash(db *gorm.DB, hash string, now time.Time) (*User, *ManagementSession, error) {
	var session ManagementSession
	if err := db.Where("token_hash = ? AND expires_at > ? AND absolute_expires_at > ?", hash, now, now).First(&session).Error; err != nil {
		return nil, nil, sessionLookupError(err)
	}
	var user User
	if err := db.First(&user, session.UserID).Error; err != nil {
		return nil, nil, sessionLookupError(err)
	}
	return &user, &session, nil
}

func sessionLookupError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

// RenewManagementSession extends an existing live session without exceeding
// its absolute deadline. A deleted or expired session can never be renewed.
func (s *Store) RenewManagementSession(hash string, idleTTL time.Duration) (*ManagementSession, error) {
	if idleTTL <= 0 {
		return nil, errors.New("session lifetime must be positive")
	}
	var session *ManagementSession
	err := s.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var err error
		_, session, err = managementSessionByHash(tx, hash, now)
		if err != nil {
			return err
		}
		session.ExpiresAt = now.Add(idleTTL)
		if session.ExpiresAt.After(session.AbsoluteExpiresAt) {
			session.ExpiresAt = session.AbsoluteExpiresAt
		}
		return tx.Model(&ManagementSession{}).Where("token_hash = ?", hash).Update("expires_at", session.ExpiresAt).Error
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// DeleteManagementSession revokes only the presented login, including when
// called again after its first successful deletion.
func (s *Store) DeleteManagementSession(hash string) error {
	if err := s.db.Where("token_hash = ?", hash).Delete(&ManagementSession{}).Error; err != nil {
		return err
	}
	s.publishManagementSession(hash, "deleted")
	return nil
}

func (s *Store) publishManagementSession(key, reason string) {
	if s.Bus != nil {
		s.Bus.PublishChange("management-session", key, reason)
	}
}
