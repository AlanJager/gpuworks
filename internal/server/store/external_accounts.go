package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrUsernameTaken: another account already has the username.
var ErrUsernameTaken = errors.New("the username is taken")

// SourceAccount is one account linked through an identity source, as the
// source's connector sees it: the link and the account's state, never its
// password hash. Protected is an administrator (role admin, the local
// administrator among them).
type SourceAccount struct {
	IdentityID uint
	Subject    string
	UserID     uint
	Username   string
	Protected  bool
	Disabled   bool
}

// CreateExternalUser creates an account with role user and no local password,
// linked to (source, subject), in one transaction (ADR 0085 decision 5). A
// taken username is ErrUsernameTaken and the account is not created; a pair
// already linked is ErrIdentityTaken.
func (s *Store) CreateExternalUser(username, source, subject string) (*User, *UserIdentity, error) {
	if !identitySourcePattern.MatchString(source) || subject == "" || len(subject) > maxIdentitySubject {
		return nil, nil, ErrInvalidIdentity
	}
	var u User
	var link UserIdentity
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&UserIdentity{}).Where("source = ? AND subject = ?", source, subject).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrIdentityTaken
		}
		if err := tx.Model(&User{}).Where("username = ?", username).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrUsernameTaken
		}
		u = User{Username: username, Role: "user"}
		// Two sign-ins racing past the reads meet the unique indexes.
		if err := tx.Create(&u).Error; err != nil {
			if errors.Is(identityWriteError(err), ErrIdentityTaken) {
				return ErrUsernameTaken
			}
			return err
		}
		link = UserIdentity{UserID: u.ID, Source: source, Subject: subject, CreatedAt: time.Now().UTC()}
		return identityWriteError(tx.Create(&link).Error)
	})
	if err != nil {
		return nil, nil, err
	}
	return &u, &link, nil
}

// LinkedAccount is the account linked to (source, subject) and the link, or
// ErrNotFound.
func (s *Store) LinkedAccount(source, subject string) (*User, *UserIdentity, error) {
	if !identitySourcePattern.MatchString(source) || subject == "" || len(subject) > maxIdentitySubject {
		return nil, nil, ErrNotFound
	}
	var link UserIdentity
	if err := s.db.Where("source = ? AND subject = ?", source, subject).First(&link).Error; err != nil {
		return nil, nil, sessionLookupError(err)
	}
	var u User
	if err := s.db.First(&u, link.UserID).Error; err != nil {
		return nil, nil, sessionLookupError(err)
	}
	return &u, &link, nil
}

// RenameUser gives an account another username; a taken one is
// ErrUsernameTaken. Sessions and keys are unaffected: they name the account
// by id.
func (s *Store) RenameUser(id uint, username string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&User{}).Where("username = ? AND id <> ?", username, id).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrUsernameTaken
		}
		res := tx.Model(&User{}).Where("id = ?", id).Update("username", username)
		if res.Error != nil {
			if errors.Is(identityWriteError(res.Error), ErrIdentityTaken) {
				return ErrUsernameTaken
			}
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RevokeUserSessions ends every management session of an account at once, as
// a sign-out from the identity source asks (ADR 0085 decision 4); its
// platform tokens and API keys are unaffected. It answers how many ended.
func (s *Store) RevokeUserSessions(id uint) (int, error) {
	var sessions []ManagementSession
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", id).Find(&sessions).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", id).Delete(&ManagementSession{}).Error
	})
	if err != nil {
		return 0, err
	}
	for _, session := range sessions {
		s.publishManagementSession(session.TokenHash, "sessions-revoked")
	}
	return len(sessions), nil
}

// AccountsBySource pages through the accounts linked through source, in link
// order: the links whose id is above after, at most limit of them (a range
// read on idx_user_identities_source, then the accounts by id). next is the
// last link id read, the after of the next page; 0 when there is no more.
func (s *Store) AccountsBySource(source string, after uint, limit int) (accounts []SourceAccount, next uint, err error) {
	if !identitySourcePattern.MatchString(source) || limit <= 0 {
		return nil, 0, ErrInvalidIdentity
	}
	var links []UserIdentity
	if err := s.reader().Where("source = ? AND id > ?", source, after).Order("id").Limit(limit).Find(&links).Error; err != nil {
		return nil, 0, err
	}
	if len(links) == 0 {
		return nil, 0, nil
	}
	ids := make([]uint, len(links))
	for i, l := range links {
		ids[i] = l.UserID
	}
	var users []User
	if err := s.reader().Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, 0, err
	}
	byID := make(map[uint]User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	out := make([]SourceAccount, 0, len(links))
	for _, l := range links {
		u, ok := byID[l.UserID]
		if !ok {
			continue
		}
		out = append(out, SourceAccount{IdentityID: l.ID, Subject: l.Subject, UserID: u.ID, Username: u.Username,
			Protected: u.Role == "admin" || u.IsLocalAdmin(), Disabled: u.DisabledAt != nil})
	}
	if len(links) < limit {
		return out, 0, nil
	}
	return out, links[len(links)-1].ID, nil
}
