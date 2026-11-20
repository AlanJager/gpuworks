package store

import (
	"context"
	"errors"
	"os"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaterialManifestRecord identifies an immutable path-to-digest inventory.
// The manifest bytes live in each member's content-addressed material store.
// This pointer alone is never evidence that a consumer path is ready.
type MaterialManifestRecord struct {
	SourceMemberID int64  `gorm:"primaryKey"`
	ClusterID      string `gorm:"size:64;not null"`
	SourceKey      string `gorm:"size:64;not null"`
	Digest         string `gorm:"size:64;not null"`
	Bytes          int64  `gorm:"not null"`
	RecordedAt     time.Time
}

// PublishMaterialManifest follows a live membership grant and authenticated
// remote byte readback. verifiedPeers contains only members whose complete
// inventory and manifest were freshly verified in this publication attempt.
// Old receipts alone never contribute to the required membership majority.
// The final pointer update rechecks the serving phase in its own transaction;
// an unchanged pointer leaves its timestamp intact after the same proof checks.
func (s *Store) PublishMaterialManifest(ctx context.Context, expected ClusterIdentity, sourceMemberID uint64, sourceKey, digest string, size int64, verifiedPeers []ClusterTransportMember) error {
	if sourceMemberID == 0 || !validClusterDigest(sourceKey, 32) || !materialDigest(digest) || size <= 0 {
		return errors.New("material manifest requires a source, digest and size")
	}
	_, members, err := s.GrantClusterMembership(ctx, expected, sourceKey)
	if err != nil {
		return err
	}
	if len(members) < 2 {
		return errors.New("material manifest needs another live member")
	}
	self := false
	currentPeers := make(map[uint64]ClusterTransportMember, len(members))
	for _, member := range members {
		if member.ID == sourceMemberID {
			self = member.KeyFingerprint == sourceKey
			continue
		}
		currentPeers[member.ID] = member
	}
	if !self {
		return errors.New("material manifest source is not a live admitted member")
	}
	seen := make(map[uint64]bool, len(verifiedPeers))
	for _, peer := range verifiedPeers {
		if current, ok := currentPeers[peer.ID]; !ok || current != peer || seen[peer.ID] {
			return errors.New("material manifest verified peer differs from live membership")
		}
		seen[peer.ID] = true
	}
	if 1+len(verifiedPeers) <= len(members)/2 {
		return errors.New("material manifest lacks a freshly verified majority")
	}
	// Acquire the writer before checking receipts. Concurrent publishers must
	// not upgrade a stale read snapshot into a write transaction.
	return s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		var local MaterialReplica
		if err := tx.Where("digest = ? AND member_id = ?", digest, int64(sourceMemberID)).First(&local).Error; err != nil {
			return err
		}
		if _, ok := readableMaterialID(ctx, local.Path, digest); !ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			return errors.New("material manifest source bytes are unavailable")
		}
		info, err := os.Lstat(local.Path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != size {
			return errors.New("material manifest source size differs")
		}
		for _, member := range verifiedPeers {
			var receipt MaterialRemoteReceipt
			if err := tx.Where("digest = ? AND member_id = ?", digest, int64(member.ID)).First(&receipt).Error; err != nil || receipt.Bytes != size {
				return errors.New("material manifest lacks a matching remote receipt")
			}
		}
		var current MaterialManifestRecord
		err = tx.Where("source_member_id = ?", int64(sourceMemberID)).Take(&current).Error
		if err == nil {
			if current.ClusterID == expected.ClusterID && current.SourceKey == sourceKey && current.Digest == digest && current.Bytes == size {
				return nil
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row := MaterialManifestRecord{SourceMemberID: int64(sourceMemberID), ClusterID: expected.ClusterID, SourceKey: sourceKey, Digest: digest, Bytes: size, RecordedAt: time.Now().UTC()}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "source_member_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"cluster_id", "source_key", "digest", "bytes", "recorded_at"}),
		}).Create(&row).Error
	})
}

func migrateMaterialManifests(db *gorm.DB) error { return db.AutoMigrate(&MaterialManifestRecord{}) }

// CountMaterialManifests reports how many source pointers this member has stored.
func (s *Store) CountMaterialManifests(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&MaterialManifestRecord{}).Count(&n).Error
	return n, err
}

// AdmittedMaterialManifestSourceKeys lists the source keys of this cluster's
// published pointers whose admission is still live. A revoked or
// never-admitted source key is left out. The read grants nothing.
func (s *Store) AdmittedMaterialManifestSourceKeys(ctx context.Context, clusterID string) ([]string, error) {
	var keys []string
	err := s.db.WithContext(ctx).Model(&MaterialManifestRecord{}).
		Joins("JOIN cluster_admissions ON cluster_admissions.key_fingerprint = material_manifest_records.source_key AND cluster_admissions.revoked_at IS NULL").
		Where("material_manifest_records.cluster_id = ?", clusterID).
		Order("material_manifest_records.source_member_id").
		Pluck("material_manifest_records.source_key", &keys).Error
	return keys, err
}

// MaterialManifestBySource returns the pointer published by one member.
// A missing row is gorm.ErrRecordNotFound.
func (s *Store) MaterialManifestBySource(ctx context.Context, sourceMemberID int64) (MaterialManifestRecord, error) {
	var row MaterialManifestRecord
	err := s.db.WithContext(ctx).Where("source_member_id = ?", sourceMemberID).Take(&row).Error
	return row, err
}
