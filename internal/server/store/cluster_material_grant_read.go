package store

import (
	"context"

	"gorm.io/gorm"
)

// This fixed private-material path holds the writer before its input read and
// observes the complete incremented record with the final phase. It does not
// cache authority or allow callers to supply either observation.
func (s *Store) grantPrivateClusterMaterials(ctx context.Context, clusterID, key string) (ClusterAdmission, error) {
	ctx, endWork := s.bindMaintenanceWork(ctx, MaintenanceWorkTransaction)
	defer endWork()
	var granted ClusterAdmission
	err := s.membershipWriteTransaction(context.WithValue(ctx, clusterGrantPurposeKey{}, "private_material_grant"), func(tx *gorm.DB) error {
		phase, identity, admission, err := readClusterMaterialGrantInput(tx, key)
		if err != nil {
			return err
		}
		if identity.ClusterID != clusterID || identity.Protocol != ClusterProtocolVersion {
			return ErrClusterIdentityConflict
		}
		granted, err = incrementClusterMaterialGrant(tx, admission)
		if err != nil {
			return err
		}
		return requireClusterMaterialGrantRecordedAndPhase(tx, ctx, granted, phase)
	})
	if err != nil {
		return ClusterAdmission{}, err
	}
	return granted, nil
}

func readClusterMaterialGrantInput(tx *gorm.DB, key string) (MaintenancePhaseState, ClusterIdentity, ClusterAdmission, error) {
	// This local scan only carries existing values and LEFT JOIN presence. A
	// missing singleton must not erase the row or skip the original check order.
	var input struct {
		Phase            MaintenancePhaseState `gorm:"embedded;embeddedPrefix:phase_"`
		Identity         ClusterIdentity       `gorm:"embedded;embeddedPrefix:identity_"`
		Admission        ClusterAdmission      `gorm:"embedded;embeddedPrefix:admission_"`
		IdentityPresent  bool
		AdmissionPresent bool
	}
	err := tx.Raw(`SELECT p.phase AS phase_phase, p.revision AS phase_revision,
		i.id IS NOT NULL AS identity_present, i.id AS identity_id,
		i.cluster_id AS identity_cluster_id, i.ca_fingerprint AS identity_ca_fingerprint,
		i.protocol AS identity_protocol, i.created_at AS identity_created_at,
		a.key_fingerprint IS NOT NULL AS admission_present,
		a.key_fingerprint AS admission_key_fingerprint, a.invitation_hash AS admission_invitation_hash,
		a.address AS admission_address, a.enrollment_origin AS admission_enrollment_origin,
		a.admitted_at AS admission_admitted_at, a.revoked_at AS admission_revoked_at,
		a.material_grant_sequence AS admission_material_grant_sequence
		FROM (SELECT 1) AS singleton
		LEFT JOIN maintenance_phase AS p ON p.id = 1
		LEFT JOIN cluster_identities AS i ON i.id = 1
		LEFT JOIN cluster_admissions AS a ON a.key_fingerprint = ? AND a.revoked_at IS NULL`, key).Scan(&input).Error
	if err != nil {
		return MaintenancePhaseState{}, ClusterIdentity{}, ClusterAdmission{}, err
	}
	if !validMaintenancePhase(input.Phase.Phase) || input.Phase.Revision <= 0 {
		return MaintenancePhaseState{}, ClusterIdentity{}, ClusterAdmission{}, ErrMaintenancePhaseUnavailable
	}
	if err := requireServingClusterPhase(input.Phase); err != nil {
		return MaintenancePhaseState{}, ClusterIdentity{}, ClusterAdmission{}, err
	}
	if !input.IdentityPresent {
		return MaintenancePhaseState{}, ClusterIdentity{}, ClusterAdmission{}, ErrClusterNotInitialized
	}
	if !input.AdmissionPresent {
		// Keep a missing/revoked caller closed, but defer that rejection until
		// the caller has performed its original cluster identity comparison.
		input.Admission = ClusterAdmission{}
	}
	return input.Phase, input.Identity, input.Admission, nil
}
