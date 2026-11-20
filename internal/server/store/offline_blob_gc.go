package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

var ErrOfflineBlobGCConflict = errors.New("offline image blob is reserved for deletion")

type OfflineBlobGCClaim struct {
	Digest     string `gorm:"primaryKey;size:64"`
	Path       string `gorm:"size:512;uniqueIndex;not null"`
	State      string `gorm:"size:16;not null"` // reserved | deleting
	Generation uint64 `gorm:"not null"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ReserveOfflineBlobForGC is the first durable exclusion step. Its reference
// check and claim insertion share the replicated scheduling transaction, so
// an image writer cannot commit a new reference between them.
func (s *Store) ReserveOfflineBlobForGC(ctx context.Context, path, digest string) error {
	path = strings.TrimSpace(path)
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || path != digest+".tar" {
		return errors.New("invalid offline blob GC identity")
	}
	return s.commitSchedulingDecision(ctx, func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&Image{}).Where("offline_blob_path = ? OR offline_blob_sha256 = ?", path, digest).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrOfflineBlobGCConflict
		}
		claim := OfflineBlobGCClaim{Digest: digest, Path: path, State: "reserved", Generation: 1}
		if err := tx.Create(&claim).Error; err != nil {
			var existing int64
			if lookupErr := tx.Model(&OfflineBlobGCClaim{}).Where("digest = ? OR path = ?", digest, path).Count(&existing).Error; lookupErr == nil && existing != 0 {
				return ErrOfflineBlobGCConflict
			}
			return err
		}
		return nil
	})
}

// MarkOfflineBlobDeleting moves only the original claim. A later filesystem
// protocol must use its generation before unlinking any member's file.
func (s *Store) MarkOfflineBlobDeleting(ctx context.Context, path, digest string, generation uint64) error {
	return s.commitSchedulingDecision(ctx, func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&Image{}).Where("offline_blob_path = ? OR offline_blob_sha256 = ?", path, digest).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrOfflineBlobGCConflict
		}
		result := tx.Model(&OfflineBlobGCClaim{}).Where("path = ? AND digest = ? AND generation = ? AND state = ?", path, digest, generation, "reserved").Update("state", "deleting")
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: claim changed before deletion", ErrOfflineBlobGCConflict)
		}
		return nil
	})
}
