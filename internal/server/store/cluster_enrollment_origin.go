package store

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"

	"gorm.io/gorm"
)

var ErrClusterEnrollmentOriginInvalid = errors.New("invalid cluster enrollment HTTPS origin")

func migrateClusterEnrollmentOrigins(db *gorm.DB) error {
	if !db.Migrator().HasTable(&ClusterAdmission{}) {
		return ErrClusterNotInitialized
	}
	if db.Migrator().HasColumn(&ClusterAdmission{}, "EnrollmentOrigin") {
		return nil
	}
	return db.Exec("ALTER TABLE cluster_admissions ADD COLUMN enrollment_origin TEXT NOT NULL DEFAULT ''").Error
}

// SetClusterEnrollmentOriginForVerifiedKey records the public HTTPS endpoint
// of an active admission. The caller must authenticate live possession of the
// member key with mTLS, verify that member certificate covers the origin host,
// and pass its verified fingerprint; this Store method cannot authenticate a
// network peer from a fingerprint string. The origin is never inferred from
// the member's database transport address. A same-origin nil result changes
// no state and is not a fresh quorum or business-readiness proof.
func (s *Store) SetClusterEnrollmentOriginForVerifiedKey(ctx context.Context, expected ClusterIdentity, keyFingerprint, origin string) error {
	_, err := s.SetClusterEnrollmentOriginForVerifiedKeyChanged(ctx, expected, keyFingerprint, origin)
	return err
}

// SetClusterEnrollmentOriginForVerifiedKeyChanged reports only a committed
// change. An identical retry has no source revision change.
func (s *Store) SetClusterEnrollmentOriginForVerifiedKeyChanged(ctx context.Context, expected ClusterIdentity, keyFingerprint, origin string) (bool, error) {
	if !validClusterDigest(keyFingerprint, 32) {
		return false, ErrClusterAdmissionRejected
	}
	normalized, err := normalizeClusterEnrollmentOrigin(origin)
	if err != nil {
		return false, err
	}
	changed := false
	err = s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		var identity ClusterIdentity
		if err := tx.First(&identity, 1).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterNotInitialized
		} else if err != nil {
			return err
		}
		if identity.ClusterID != expected.ClusterID || identity.CAFingerprint != expected.CAFingerprint || identity.Protocol != expected.Protocol {
			return ErrClusterIdentityConflict
		}
		var admission ClusterAdmission
		if err := tx.Where("key_fingerprint = ? AND revoked_at IS NULL", keyFingerprint).First(&admission).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrClusterAdmissionRejected
		} else if err != nil {
			return err
		}
		address, err := normalizeClusterMemberAddress(admission.Address)
		if err != nil || address != admission.Address {
			return ErrClusterAdmissionRejected
		}
		if admission.EnrollmentOrigin == normalized {
			return nil
		}
		result := tx.Model(&ClusterAdmission{}).
			Where("key_fingerprint = ? AND address = ? AND revoked_at IS NULL", keyFingerprint, address).
			Update("enrollment_origin", normalized)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrClusterAdmissionRejected
		}
		// Membership and transport policy snapshots share this source
		// revision. A changed origin must never appear under an older one.
		if err := advanceClusterAuthorization(tx); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func normalizeClusterEnrollmentOrigin(origin string) (string, error) {
	if origin == "" || strings.TrimSpace(origin) != origin || strings.Contains(origin, "#") {
		return "", ErrClusterEnrollmentOriginInvalid
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "" && u.Path != "/") || strings.HasSuffix(u.Host, ":") {
		return "", ErrClusterEnrollmentOriginInvalid
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	address, err := normalizeClusterMemberAddress(net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", ErrClusterEnrollmentOriginInvalid
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", ErrClusterEnrollmentOriginInvalid
	}
	if port == "443" {
		if strings.Contains(host, ":") {
			return "https://[" + host + "]", nil
		}
		return "https://" + host, nil
	}
	return "https://" + address, nil
}
