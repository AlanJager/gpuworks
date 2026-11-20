package store

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// ReadLatestActionGrantRoster reads replicated roster bytes for reconciliation.
// Database contents alone do not authenticate an agent trust transition.
func (s *Store) ReadLatestActionGrantRoster(ctx context.Context, clusterID string) (proto.TrustedActionRoster, error) {
	var empty proto.TrustedActionRoster
	if !validClusterDigest(clusterID, 16) {
		return empty, ErrClusterIdentityConflict
	}
	var roster proto.TrustedActionRoster
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireClusterIdentity(tx, clusterID); err != nil {
			return err
		}
		var header ActionGrantRosterRecord
		if err := tx.Where("cluster_id = ?", clusterID).Order("revision DESC").First(&header).Error; err != nil {
			return err
		}
		if header.Revision <= 0 || header.NotAfterUnixMS <= 0 {
			return errors.New("invalid replicated action roster header")
		}
		var rows []ActionGrantRosterMemberRecord
		if err := tx.Where("cluster_id = ? AND revision = ?", clusterID, header.Revision).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 || len(rows) > 512 {
			return errors.New("invalid replicated action roster size")
		}
		roster = proto.TrustedActionRoster{ClusterID: clusterID, Revision: uint64(header.Revision), NotAfterUnixMS: header.NotAfterUnixMS}
		ids := make(map[uint64]bool, len(rows))
		keys := make(map[string]bool, len(rows))
		voters := 0
		for _, row := range rows {
			id := uint64(row.MemberID)
			public := ed25519.PublicKey(row.PublicKey)
			if id == 0 || ids[id] || len(public) != ed25519.PublicKeySize || keys[row.KeyID] {
				return errors.New("invalid replicated action roster member")
			}
			switch row.Role {
			case "voter":
				voters++
			case "spare", "stand-by":
			default:
				return errors.New("invalid replicated action roster role")
			}
			spki, err := x509.MarshalPKIXPublicKey(public)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(spki)
			if row.KeyID != hex.EncodeToString(sum[:]) {
				return errors.New("replicated action roster key fingerprint differs from public key")
			}
			ids[id], keys[row.KeyID] = true, true
			roster.Members = append(roster.Members, proto.TrustedActionMember{ID: id, Role: row.Role, KeyID: row.KeyID, PublicKey: append(ed25519.PublicKey(nil), public...), Revoked: row.Revoked})
		}
		if voters == 0 {
			return errors.New("replicated action roster has no voter")
		}
		slices.SortFunc(roster.Members, func(a, b proto.TrustedActionMember) int { return cmp.Compare(a.ID, b.ID) })
		return nil
	})
	if err != nil {
		return empty, err
	}
	return roster, nil
}

// PublishActionGrantRoster accepts certificates gathered from current pinned
// member TLS handshakes. The caller must authenticate the requester and pass
// the exact membership snapshot it used to dial those members. Both grants
// commit separate majority-confirmed writes; a change or unknown result
// returns no usable roster. A row left by an unknown result is never an agent
// trust anchor and must be reconciled by revision before another publication.
func (s *Store) PublishActionGrantRoster(ctx context.Context, expected ClusterIdentity, requesterKey string, observed []ClusterTransportMember, certificates []*x509.Certificate, trust *identity.Trust, previousRevision uint64, notAfter time.Time) (proto.TrustedActionRoster, error) {
	var empty proto.TrustedActionRoster
	_, before, err := s.GrantClusterMembership(ctx, expected, requesterKey)
	if err != nil {
		return empty, err
	}
	if !slices.Equal(before, observed) {
		return empty, errors.New("action grant roster membership changed before publication")
	}
	roster, err := s.writeActionGrantRoster(ctx, expected, before, certificates, trust, previousRevision, notAfter)
	if err != nil {
		return empty, err
	}
	_, after, err := s.GrantClusterMembership(ctx, expected, requesterKey)
	if err != nil {
		return empty, err
	}
	if !slices.Equal(before, after) {
		return empty, errors.New("action grant roster membership changed after publication")
	}
	return roster, nil
}

// writeActionGrantRoster is the replicated CAS portion of roster publication.
// Its caller must obtain members from a fresh, majority-confirmed live Raft
// observation and authenticate the requester. Persisting this row does not
// install or certify an agent trust transition.
func (s *Store) writeActionGrantRoster(ctx context.Context, expected ClusterIdentity, members []ClusterTransportMember, certificates []*x509.Certificate, trust *identity.Trust, previousRevision uint64, notAfter time.Time) (proto.TrustedActionRoster, error) {
	var empty proto.TrustedActionRoster
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || !trust.MatchesIdentity(expected.ClusterID, expected.CAFingerprint) || expected.Protocol != ClusterProtocolVersion ||
		len(members) == 0 || len(members) > 512 || len(certificates) != len(members) || previousRevision >= math.MaxInt64 {
		return empty, ErrClusterIdentityConflict
	}
	now := time.Now().UTC()
	if !notAfter.After(now) || notAfter.UnixMilli() <= 0 {
		return empty, errors.New("action grant roster expiry must be in the future")
	}
	byKey := make(map[string]ed25519.PublicKey, len(certificates))
	for _, certificate := range certificates {
		fingerprint, err := trust.VerifyMember(certificate, now)
		if err != nil {
			return empty, err
		}
		if _, exists := byKey[fingerprint]; exists || !notAfter.Before(certificate.NotAfter) {
			return empty, errors.New("duplicate or expiring action grant member certificate")
		}
		publicKey := certificate.PublicKey.(ed25519.PublicKey)
		byKey[fingerprint] = append(ed25519.PublicKey(nil), publicKey...)
	}
	roster := proto.TrustedActionRoster{ClusterID: expected.ClusterID, Revision: previousRevision + 1, NotAfterUnixMS: notAfter.UnixMilli()}
	ids := make(map[uint64]bool, len(members))
	voters := 0
	for _, member := range members {
		key := byKey[member.KeyFingerprint]
		if member.ID == 0 || ids[member.ID] || len(key) != ed25519.PublicKeySize {
			return empty, errors.New("action grant member differs from authenticated certificate")
		}
		switch member.Role {
		case "voter":
			voters++
		case "spare", "stand-by":
		default:
			return empty, errors.New("invalid action grant member role")
		}
		ids[member.ID] = true
		roster.Members = append(roster.Members, proto.TrustedActionMember{ID: member.ID, Role: member.Role, KeyID: member.KeyFingerprint, PublicKey: key})
	}
	if voters == 0 || len(ids) != len(byKey) {
		return empty, errors.New("action grant roster lacks voters or has unmatched certificates")
	}
	err := s.servingClusterWriteTransaction(ctx, func(tx *gorm.DB) error {
		var actual ClusterIdentity
		if err := tx.First(&actual, 1).Error; err != nil {
			return err
		}
		if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
			return ErrClusterIdentityConflict
		}
		var latest int64
		if err := tx.Model(&ActionGrantRosterRecord{}).Where("cluster_id = ?", expected.ClusterID).Select("COALESCE(MAX(revision), 0)").Scan(&latest).Error; err != nil {
			return err
		}
		if latest < 0 || uint64(latest) != previousRevision {
			return errors.New("action grant roster revision changed")
		}
		for _, member := range members {
			var admission ClusterAdmission
			if err := tx.Where("key_fingerprint = ? AND revoked_at IS NULL", member.KeyFingerprint).First(&admission).Error; err != nil {
				return ErrClusterAdmissionRejected
			}
			if admission.Address != member.Address {
				return ErrClusterAdmissionRejected
			}
		}
		record := ActionGrantRosterRecord{ClusterID: expected.ClusterID, Revision: int64(roster.Revision), NotAfterUnixMS: roster.NotAfterUnixMS, CreatedAt: now}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		for _, member := range roster.Members {
			record := ActionGrantRosterMemberRecord{ClusterID: expected.ClusterID, Revision: int64(roster.Revision), MemberID: int64(member.ID), Role: member.Role, KeyID: member.KeyID, PublicKey: []byte(member.PublicKey)}
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return empty, err
	}
	return roster, nil
}
