package store

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"gorm.io/gorm"
)

// Errors of the account and external-identity operations (ADR 0085).
var (
	// ErrUserDisabled: the account is disabled; sign-in, platform tokens and
	// inference API keys of this user are refused (USER_DISABLED).
	ErrUserDisabled = errors.New("user is disabled")
	// ErrLastLocalAdmin: disabling this account would leave no enabled local
	// administrator, the floor of ADR 0071 decision 11.
	ErrLastLocalAdmin = errors.New("the last enabled local administrator cannot be disabled")
	// ErrIdentityTaken: the (source, subject) pair is already linked, to this
	// user or another one.
	ErrIdentityTaken = errors.New("the external identity is already linked")
)

// identitySourcePattern is the source-name rule: one word the extension owns,
// the same shape as module and plugin names.
var identitySourcePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// maxIdentitySubject bounds a subject: an id from an identity source, never a
// free-form description.
const maxIdentitySubject = 256

// UserByID is one account, disabled or not.
func (s *Store) UserByID(id uint) (*User, error) {
	var u User
	if err := s.reader().First(&u, id).Error; err != nil {
		return nil, sessionLookupError(err)
	}
	return &u, nil
}

// ListUsers is every account in id order, disabled ones included: an
// administrator manages the accounts the platform holds.
func (s *Store) ListUsers() ([]User, error) {
	var out []User
	if err := s.reader().Order("id").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// FindUserByIdentity answers the account linked to (source, subject), or
// ErrNotFound. It is how an identity connector signs a user in (ADR 0085
// decision 5): the pair is the only key, never a username or an email.
func (s *Store) FindUserByIdentity(source, subject string) (*User, error) {
	if !identitySourcePattern.MatchString(source) || subject == "" || len(subject) > maxIdentitySubject {
		return nil, ErrNotFound
	}
	var identity UserIdentity
	if err := s.reader().Where("source = ? AND subject = ?", source, subject).First(&identity).Error; err != nil {
		return nil, sessionLookupError(err)
	}
	return s.UserByID(identity.UserID)
}

// ExternalIdentities is the accounts' links to outside identity sources, in
// creation order.
func (s *Store) ExternalIdentities(userID uint) ([]UserIdentity, error) {
	var out []UserIdentity
	if err := s.reader().Where("user_id = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// LinkIdentity links an existing account to (source, subject). Linking never
// creates an account and never changes its role: an identity connector cannot
// make anyone an administrator (ADR 0085 decision 5).
func (s *Store) LinkIdentity(userID uint, source, subject string) (*UserIdentity, error) {
	if !identitySourcePattern.MatchString(source) {
		return nil, fmt.Errorf("identity source %q: %w", source, ErrInvalidIdentity)
	}
	if subject == "" || len(subject) > maxIdentitySubject {
		return nil, fmt.Errorf("identity subject length %d: %w", len(subject), ErrInvalidIdentity)
	}
	var out UserIdentity
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var u User
		if err := tx.First(&u, userID).Error; err != nil {
			return sessionLookupError(err)
		}
		var existing UserIdentity
		err := tx.Where("source = ? AND subject = ?", source, subject).First(&existing).Error
		if err == nil {
			return ErrIdentityTaken
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		out = UserIdentity{UserID: userID, Source: source, Subject: subject, CreatedAt: time.Now().UTC()}
		return tx.Create(&out).Error
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrInvalidIdentity: the source name or the subject does not have the shape
// of an external identity.
var ErrInvalidIdentity = errors.New("invalid external identity")

// UnlinkIdentity removes one of the account's links. A link of another
// account is ErrNotFound, so an administrator cannot take one away by
// guessing its id.
func (s *Store) UnlinkIdentity(userID, identityID uint) error {
	res := s.db.Where("id = ? AND user_id = ?", identityID, userID).Delete(&UserIdentity{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableUser disables an account (ADR 0085 decision 7): its sessions are
// deleted at once, sign-in, platform tokens and inference API keys are refused
// with USER_DISABLED, and its data and keys are kept. Running workloads are
// not interrupted. Disabling an already disabled account changes nothing.
func (s *Store) DisableUser(id uint) (*User, error) {
	var out User
	var sessions []ManagementSession
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; err != nil {
			return sessionLookupError(err)
		}
		if out.DisabledAt != nil {
			return nil
		}
		if out.Role == "admin" && out.LocalAdmin {
			var remaining int64
			if err := tx.Model(&User{}).
				Where("role = ? AND local_admin = ? AND disabled_at IS NULL AND id <> ?", "admin", true, id).
				Count(&remaining).Error; err != nil {
				return err
			}
			if remaining == 0 {
				return ErrLastLocalAdmin
			}
		}
		now := time.Now().UTC()
		if err := tx.Model(&User{}).Where("id = ?", id).Update("disabled_at", now).Error; err != nil {
			return err
		}
		out.DisabledAt = &now
		if err := tx.Where("user_id = ?", id).Find(&sessions).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", id).Delete(&ManagementSession{}).Error
	})
	if err != nil {
		return nil, err
	}
	// Publishing after the commit: an open stream re-checks its credential
	// on this topic and ends before it forwards another change.
	for _, session := range sessions {
		s.publishManagementSession(session.TokenHash, "user-disabled")
	}
	// New requests in this process see the disablement at once; other
	// processes follow within their caches' TTLs (ADR 0085 decision 7).
	s.apiKeys.forgetUser(id)
	s.platformTokens.forgetUser(id)
	return &out, nil
}

// EnableUser restores a disabled account; its data and keys work again.
// Enabling an already enabled account changes nothing.
func (s *Store) EnableUser(id uint) (*User, error) {
	var out User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; err != nil {
			return sessionLookupError(err)
		}
		if out.DisabledAt == nil {
			return nil
		}
		if err := tx.Model(&User{}).Where("id = ?", id).Update("disabled_at", nil).Error; err != nil {
			return err
		}
		out.DisabledAt = nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.apiKeys.forgetUser(id)
	s.platformTokens.forgetUser(id)
	return &out, nil
}
