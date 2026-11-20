package store

import (
	"context"
	"math"

	"gorm.io/gorm"
)

// GrantClusterMaterials authorizes one private-material response only after a
// fresh replicated write commits. The caller must first prove possession of
// the member private key through authenticated TLS, never a CSR or bearer.
// A read-only admission check can succeed during a quorum-loss window. A
// same-value UPDATE can be optimized away by SQLite and is not a substitute.
// Failures, including unknown commit outcomes, return no authorization.
func (s *Store) GrantClusterMaterials(ctx context.Context, clusterID, keyFingerprint string) (ClusterAdmission, error) {
	if !validClusterDigest(keyFingerprint, 32) {
		return ClusterAdmission{}, ErrClusterAdmissionRejected
	}
	return s.grantPrivateClusterMaterials(ctx, clusterID, keyFingerprint)
}

func grantClusterMaterials(tx *gorm.DB, admission ClusterAdmission) (ClusterAdmission, error) {
	expected, err := incrementClusterMaterialGrant(tx, admission)
	if err != nil {
		return ClusterAdmission{}, err
	}
	if err := requireClusterMaterialGrantRecorded(tx, expected); err != nil {
		return ClusterAdmission{}, err
	}
	return expected, nil
}

// The complete recorded check follows this actual mutation in every caller.
// Transport checks before its snapshot; private reads the record with phase.
func incrementClusterMaterialGrant(tx *gorm.DB, admission ClusterAdmission) (ClusterAdmission, error) {
	if admission.KeyFingerprint == "" || admission.RevokedAt != nil {
		return ClusterAdmission{}, ErrClusterAdmissionRejected
	}
	if admission.MaterialGrantSequence < 0 || admission.MaterialGrantSequence == math.MaxInt64 {
		return ClusterAdmission{}, ErrClusterAdmissionRejected
	}
	expected := admission
	expected.MaterialGrantSequence++
	result := tx.Model(&ClusterAdmission{}).
		Where("key_fingerprint = ? AND revoked_at IS NULL AND material_grant_sequence = ?", admission.KeyFingerprint, admission.MaterialGrantSequence).
		UpdateColumn("material_grant_sequence", admission.MaterialGrantSequence+1)
	if result.Error != nil {
		return ClusterAdmission{}, result.Error
	}
	if result.RowsAffected != 1 {
		return ClusterAdmission{}, ErrClusterAdmissionRejected
	}
	return expected, nil
}

// The grant must retain the complete admitted identity and exactly one sequence
// increment. RowsAffected alone cannot reveal a trigger rewriting that row.
func requireClusterMaterialGrantRecorded(tx *gorm.DB, expected ClusterAdmission) error {
	var actual ClusterAdmission
	if err := tx.Where("key_fingerprint = ?", expected.KeyFingerprint).First(&actual).Error; err != nil {
		return err
	}
	return requireClusterMaterialGrantValue(expected, actual)
}

func requireClusterMaterialGrantValue(expected, actual ClusterAdmission) error {
	expected.AdmittedAt = expected.AdmittedAt.UTC()
	actual.AdmittedAt = actual.AdmittedAt.UTC()
	if actual != expected {
		return ErrClusterAdmissionRejected
	}
	return nil
}
