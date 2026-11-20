package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// HasAnyAPIKeyContext observes configured keys without granting request access.
func (s *Store) HasAnyAPIKeyContext(ctx context.Context) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var count int64
	if err := s.reader().WithContext(ctx).Model(&APIKey{}).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// AuthenticateAPIKey admits a configured key only after its last-used write
// commits. With no configured keys, the existing development policy admits a
// request after rechecking absence under the business transaction's phase
// fence. That absence check does not establish quorum authority.
func (s *Store) AuthenticateAPIKey(ctx context.Context, presented string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	has, err := s.HasAnyAPIKeyContext(ctx)
	if err != nil {
		return false, err
	}
	if has {
		return s.verifyAPIKey(ctx, presented)
	}
	allowed := false
	err = s.withBusinessWriteTx(ctx, func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&APIKey{}).Count(&count).Error; err != nil {
			return err
		}
		allowed = count == 0
		return nil
	})
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return allowed, nil
}

func (s *Store) verifyAPIKey(ctx context.Context, presented string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var k APIKey
	err := s.reader().WithContext(ctx).Where("prefix = ?", auth.PrefixOf(presented)).First(&k).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Argon verification runs before acquiring the writer or business owner.
	valid := auth.VerifyPassword(presented, k.Hash)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !valid {
		return false, nil
	}
	accepted := false
	err = s.withBusinessWriteTx(ctx, func(tx *gorm.DB) error {
		res := tx.Model(&APIKey{}).
			Where("id = ? AND prefix = ? AND hash = ? AND user_id = ?", k.ID, k.Prefix, k.Hash, k.UserID).
			Update("last_used", time.Now().UTC())
		if res.Error != nil {
			return res.Error
		}
		accepted = res.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return accepted, nil
}
