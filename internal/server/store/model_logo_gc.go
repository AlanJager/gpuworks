package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

var (
	ErrLogoGCInvalid    = errors.New("invalid logo GC identity")
	ErrLogoGCDeleting   = errors.New("logo digest is deleting")
	ErrLogoGCStale      = errors.New("logo GC token is stale or expired")
	ErrLogoGCReferenced = errors.New("logo digest has a model reference or live reservation")
)

type LogoReservation struct {
	Name       string
	Owner      string
	Token      string
	Generation int64
}

type LogoDeletion struct {
	Name       string
	Token      string
	Generation int64
}

func logoDigest(name string) (string, error) {
	if len(name) != 68 && len(name) != 69 {
		return "", ErrLogoGCInvalid
	}
	ext := name[64:]
	if ext != ".png" && ext != ".jpg" && ext != ".gif" && ext != ".webp" {
		return "", ErrLogoGCInvalid
	}
	for _, c := range name[:64] {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return "", ErrLogoGCInvalid
		}
	}
	return name[:64], nil
}

func logoToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func logoNow(tx *gorm.DB) (int64, error) {
	var row struct{ Now int64 }
	err := tx.Raw(`SELECT CAST(strftime('%s','now') AS integer) AS now`).Scan(&row).Error
	return row.Now, err
}

// ReserveModelLogo admits a prospective reference. A caller must publish the
// bytes and then call CheckModelLogoReservation inside the committing model
// transaction. Owner identifies the operation for recovery and diagnostics.
func (s *Store) ReserveModelLogo(ctx context.Context, name, owner string, ttl time.Duration) (LogoReservation, error) {
	r, err := s.reserveModelLogoAttempt(ctx, name, owner, ttl)
	if err != nil {
		return LogoReservation{}, err
	}
	return r, nil
}

// reserveModelLogoAttempt retains this attempt's generated identity on error
// solely for exact cancellation. A failed or unknown commit is never authority
// to publish, even when the tuple was fully constructed before the error.
func (s *Store) reserveModelLogoAttempt(ctx context.Context, name, owner string, ttl time.Duration) (LogoReservation, error) {
	digest, err := logoDigest(name)
	if err != nil || strings.TrimSpace(owner) == "" || ttl <= 0 {
		return LogoReservation{}, ErrLogoGCInvalid
	}
	token, err := logoToken()
	if err != nil {
		return LogoReservation{}, err
	}
	r := LogoReservation{Name: name, Owner: owner, Token: token}
	err = s.withBusinessWriteTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT OR IGNORE INTO model_logo_gc (digest) VALUES (?)`, digest).Error; err != nil {
			return err
		}
		now, err := logoNow(tx)
		if err != nil {
			return err
		}
		// First write serializes admission against marking and avoids snapshot promotion.
		result := tx.Exec(`UPDATE model_logo_gc SET generation = generation WHERE digest = ? AND deleting_name = ''`, digest)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLogoGCDeleting
		}
		if err := tx.Raw(`SELECT generation FROM model_logo_gc WHERE digest = ?`, digest).Scan(&r.Generation).Error; err != nil {
			return err
		}
		seconds := int64((ttl + time.Second - 1) / time.Second)
		return tx.Exec(`INSERT INTO model_logo_reservations (token,digest,name,owner,generation,expires_at) VALUES (?,?,?,?,?,?)`, token, digest, name, owner, r.Generation, now+seconds).Error
	})
	return r, err
}

// CheckModelLogoReservation must be the first write in the same transaction
// that creates/updates logo_path. A separate read or preflight is insufficient.
func CheckModelLogoReservation(tx *gorm.DB, r LogoReservation) error {
	digest, err := logoDigest(r.Name)
	if err != nil || r.Token == "" || r.Owner == "" || r.Generation < 1 {
		return ErrLogoGCInvalid
	}
	result := tx.Exec(`UPDATE model_logo_gc SET generation = generation WHERE digest = ? AND generation = ? AND deleting_name = ''`, digest, r.Generation)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLogoGCStale
	}
	now, err := logoNow(tx)
	if err != nil {
		return err
	}
	var count int64
	if err := tx.Raw(`SELECT count(*) FROM model_logo_reservations WHERE token = ? AND digest = ? AND name = ? AND owner = ? AND generation = ? AND expires_at > ?`, r.Token, digest, r.Name, r.Owner, r.Generation, now).Scan(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return ErrLogoGCStale
	}
	// Consume the ticket with the reference write. Rollback restores it.
	return tx.Exec(`DELETE FROM model_logo_reservations WHERE token = ?`, r.Token).Error
}

// MarkModelLogoDeleting wins only while no model references or unexpired
// admission tickets exist. The tombstone survives restarts until completion.
func (s *Store) MarkModelLogoDeleting(ctx context.Context, name string) (LogoDeletion, error) {
	digest, err := logoDigest(name)
	if err != nil {
		return LogoDeletion{}, err
	}
	token, err := logoToken()
	if err != nil {
		return LogoDeletion{}, err
	}
	d := LogoDeletion{Name: name, Token: token}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT OR IGNORE INTO model_logo_gc (digest) VALUES (?)`, digest).Error; err != nil {
			return err
		}
		now, err := logoNow(tx)
		if err != nil {
			return err
		}
		result := tx.Exec(`UPDATE model_logo_gc SET deleting_name = ?, deletion_token = ? WHERE digest = ? AND deleting_name = '' AND NOT EXISTS (SELECT 1 FROM models WHERE logo_path LIKE ? OR logo_path = ?) AND NOT EXISTS (SELECT 1 FROM model_logo_reservations WHERE digest = ? AND expires_at > ?)`, name, token, digest, digest+".%", name, digest, now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLogoGCReferenced
		}
		return tx.Raw(`SELECT generation FROM model_logo_gc WHERE digest = ?`, digest).Scan(&d.Generation).Error
	})
	if err != nil {
		return LogoDeletion{}, err
	}
	return d, nil
}

// DeletingModelLogos exposes durable tombstones for restart reconciliation.
func (s *Store) DeletingModelLogos(ctx context.Context) ([]LogoDeletion, error) {
	var rows []struct {
		Name       string
		Token      string
		Generation int64
	}
	err := s.db.WithContext(ctx).Raw(`SELECT deleting_name AS name, deletion_token AS token, generation FROM model_logo_gc WHERE deleting_name <> '' ORDER BY digest`).Scan(&rows).Error
	out := make([]LogoDeletion, 0, len(rows))
	for _, row := range rows {
		out = append(out, LogoDeletion{Name: row.Name, Token: row.Token, Generation: row.Generation})
	}
	return out, err
}

// CompleteModelLogoDeletion reopens admission after unlink and directory sync.
// The cleaner must never call this before those file operations succeed.
func (s *Store) CompleteModelLogoDeletion(ctx context.Context, d LogoDeletion) error {
	digest, err := logoDigest(d.Name)
	if err != nil || d.Token == "" || d.Generation < 1 {
		return ErrLogoGCInvalid
	}
	result := s.db.WithContext(ctx).Exec(`UPDATE model_logo_gc SET deleting_name = '', deletion_token = '', generation = generation + 1 WHERE digest = ? AND deleting_name = ? AND deletion_token = ? AND generation = ?`, digest, d.Name, d.Token, d.Generation)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLogoGCStale
	}
	return nil
}

// CancelModelLogoReservation drops an unused ticket; expiry is also sufficient.
func (s *Store) CancelModelLogoReservation(ctx context.Context, r LogoReservation) error {
	digest, err := logoDigest(r.Name)
	if err != nil {
		return err
	}
	result := s.db.WithContext(ctx).Exec(`DELETE FROM model_logo_reservations WHERE token = ? AND digest = ? AND name = ? AND owner = ? AND generation = ?`, r.Token, digest, r.Name, r.Owner, r.Generation)
	if result.Error != nil {
		return fmt.Errorf("cancel logo reservation: %w", result.Error)
	}
	return nil
}

// RenewModelLogoReservation extends a live ticket during slow publication.
// Expired tickets cannot be revived, even if the digest is currently open.
func (s *Store) RenewModelLogoReservation(ctx context.Context, r LogoReservation, ttl time.Duration) error {
	digest, err := logoDigest(r.Name)
	if err != nil || r.Token == "" || r.Owner == "" || r.Generation < 1 || ttl <= 0 {
		return ErrLogoGCInvalid
	}
	return s.withBusinessWriteTx(ctx, func(tx *gorm.DB) error {
		result := tx.Exec(`UPDATE model_logo_gc SET generation = generation WHERE digest = ? AND generation = ? AND deleting_name = ''`, digest, r.Generation)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLogoGCStale
		}
		now, err := logoNow(tx)
		if err != nil {
			return err
		}
		seconds := int64((ttl + time.Second - 1) / time.Second)
		result = tx.Exec(`UPDATE model_logo_reservations SET expires_at = ? WHERE token = ? AND digest = ? AND name = ? AND owner = ? AND generation = ? AND expires_at > ?`, now+seconds, r.Token, digest, r.Name, r.Owner, r.Generation, now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLogoGCStale
		}
		return nil
	})
}
