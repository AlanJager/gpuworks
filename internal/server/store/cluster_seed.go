package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// InitializeClusterSeedAdmission is exclusively for the explicit offline seed
// operation, after proving the established member and its cluster authority.
// The empty invitation hash reserves the existing unique slot for that member;
// normal admission always requires a real invitation's SHA256 hash.
func (s *Store) InitializeClusterSeedAdmission(ctx context.Context, clusterID, key, address string) (ClusterAdmission, error) {
	if !validClusterDigest(key, 32) {
		return ClusterAdmission{}, ErrClusterAdmissionRejected
	}
	address, err := normalizeClusterMemberAddress(address)
	if err != nil {
		return ClusterAdmission{}, err
	}
	var result ClusterAdmission
	err = s.clusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		err := tx.Where("invitation_hash = ?", "").First(&result).Error
		if err == nil {
			if result.KeyFingerprint != key || result.Address != address || result.RevokedAt != nil {
				return ErrClusterAdmissionRejected
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var conflicts int64
		if err := tx.Model(&ClusterAdmission{}).Where("key_fingerprint = ? OR (address = ? AND revoked_at IS NULL)", key, address).Count(&conflicts).Error; err != nil {
			return err
		}
		if conflicts != 0 {
			return ErrClusterAdmissionRejected
		}
		result = ClusterAdmission{KeyFingerprint: key, Address: address, AdmittedAt: tx.NowFunc().UTC()}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		return advanceClusterAuthorization(tx)
	})
	if err != nil {
		return ClusterAdmission{}, err
	}
	return result, nil
}

// ClusterTransportAdmissions reads one coherent authorization input snapshot.
// It includes the seed and revoked tombstones. This read alone neither proves
// quorum nor publishes a live transport permission; callers must supply their
// own authorization and durable publication protocol.
func (s *Store) ClusterTransportAdmissions(ctx context.Context, clusterID string) ([]ClusterAdmission, error) {
	var result []ClusterAdmission
	err := s.clusterTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		var err error
		result, err = clusterTransportAdmissions(tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func clusterTransportAdmissions(tx *gorm.DB) ([]ClusterAdmission, error) {
	const maximumAdmissions = 512
	var result []ClusterAdmission
	if err := tx.Order("key_fingerprint").Limit(maximumAdmissions + 1).Find(&result).Error; err != nil {
		return nil, err
	}
	if err := validateClusterTransportAdmissions(result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateClusterTransportAdmissions(result []ClusterAdmission) error {
	const maximumAdmissions = 512
	if len(result) > maximumAdmissions {
		return errors.New("cluster admission snapshot exceeds transport policy capacity")
	}
	seeds := 0
	for _, row := range result {
		if !validClusterDigest(row.KeyFingerprint, 32) {
			return ErrClusterAdmissionRejected
		}
		address, err := normalizeClusterMemberAddress(row.Address)
		if err != nil || address != row.Address {
			return ErrClusterAdmissionRejected
		}
		if row.InvitationHash == "" {
			seeds++
		} else if !validClusterDigest(row.InvitationHash, 32) {
			return ErrClusterAdmissionRejected
		}
	}
	if seeds != 1 {
		return errors.New("cluster seed admission is missing")
	}
	return nil
}
