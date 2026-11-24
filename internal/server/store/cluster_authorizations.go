package store

import (
	"context"
	"errors"
	"math"

	"gorm.io/gorm"
)

func advanceClusterAuthorization(tx *gorm.DB) error {
	result := tx.Exec(`UPDATE cluster_authorizations SET revision = revision + 1 WHERE id = 1 AND typeof(revision) = 'integer' AND revision >= 0 AND revision < ?`, int64(math.MaxInt64))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("cluster authorization revision is missing, invalid or exhausted")
	}
	return nil
}

// RevokeClusterAdmission retains a permanent key tombstone. This database
// transition alone does not disconnect existing TLS sessions, remove a Raft
// member, or install the new authorization policy on other members. The caller
// must separately authorize this administrative operation.
func (s *Store) RevokeClusterAdmission(ctx context.Context, clusterID, key string) (ClusterAdmission, error) {
	if !validClusterDigest(key, 32) {
		return ClusterAdmission{}, ErrClusterAdmissionRejected
	}
	var admission ClusterAdmission
	err := s.membershipWriteTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		err := tx.Where("key_fingerprint = ?", key).First(&admission).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterAdmissionRejected
		}
		if err != nil {
			return err
		}
		if admission.RevokedAt != nil {
			return nil
		}
		now := tx.NowFunc().UTC()
		result := tx.Model(&ClusterAdmission{}).Where("key_fingerprint = ? AND revoked_at IS NULL", key).UpdateColumn("revoked_at", now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrClusterAdmissionRejected
		}
		if err := advanceClusterAuthorization(tx); err != nil {
			return err
		}
		admission.RevokedAt = &now
		return nil
	})
	if err != nil {
		return ClusterAdmission{}, err
	}
	// The member that committed a revocation knows it without asking: it
	// drops its confirmed list, so a connection from the revoked member is
	// decided by a fresh grant, or refused when none can be had.
	s.ForgetConfirmedClusterMembership()
	return admission, nil
}

// GrantClusterTransportAdmissions requires proof of caller key possession at
// the transport boundary. A distinct replicated material grant and the complete
// versioned snapshot share one transaction; an unconfirmed commit returns no
// snapshot. The source revision is not a local policy-file CAS revision.
func (s *Store) GrantClusterTransportAdmissions(ctx context.Context, expected ClusterIdentity, key string) (uint64, []ClusterAdmission, error) {
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return 0, nil, ErrClusterIdentityConflict
	}
	if !validClusterDigest(key, 32) {
		return 0, nil, ErrClusterAdmissionRejected
	}
	return s.grantClusterTransportAdmissionKeys(ctx, expected, []string{key})
}
