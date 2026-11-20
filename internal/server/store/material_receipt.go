package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecordMaterialRemoteReceipts atomically indexes one fully verified upload
// batch for one peer. Receipts remain hints, never authority for remote bytes;
// callers must retain each file's fresh authorization before this transaction.
func (s *Store) RecordMaterialRemoteReceipts(ctx context.Context, receipts []MaterialRemoteReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(receipts) == 0 {
		return nil
	}
	// Upload readback groups use the existing legacy bound of 32 files.
	if len(receipts) > 32 {
		return errors.New("remote material receipt batch exceeds upload readback bound")
	}
	member := receipts[0].MemberID
	rows := make([]MaterialRemoteReceipt, 0, len(receipts))
	digests := make([]string, 0, len(receipts))
	seen := make(map[string]bool, len(receipts))
	now := time.Now().UTC()
	for _, receipt := range receipts {
		if !materialDigest(receipt.Digest) || member == 0 || receipt.MemberID != member || receipt.Bytes <= 0 || seen[receipt.Digest] {
			return errors.New("remote material receipt batch requires distinct digests, one member, and positive sizes")
		}
		seen[receipt.Digest] = true
		digests = append(digests, receipt.Digest)
		rows = append(rows, MaterialRemoteReceipt{Digest: receipt.Digest, MemberID: member, Bytes: receipt.Bytes, RecordedAt: now})
	}
	return s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
			return err
		}
		var existing []MaterialRemoteReceipt
		if err := tx.Where("member_id = ? AND digest IN ?", member, digests).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) != len(rows) {
			return errors.New("remote material receipt batch is incomplete")
		}
		sizes := make(map[string]int64, len(rows))
		for _, row := range rows {
			sizes[row.Digest] = row.Bytes
		}
		for _, row := range existing {
			if row.Bytes != sizes[row.Digest] {
				return errors.New("remote material receipt contradicts recorded size")
			}
		}
		return nil
	})
}
