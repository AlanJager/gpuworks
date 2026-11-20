package store

import "time"

// ClusterIdentity is explicitly initialized once, never synthesized by reads.
// The CA fingerprint binds invitations to the authenticated cluster transport.
type ClusterIdentity struct {
	ID            uint   `gorm:"primaryKey;autoIncrement:false;check:cluster_identity_singleton,id = 1"`
	ClusterID     string `gorm:"size:32;not null;uniqueIndex"`
	CAFingerprint string `gorm:"size:64;not null"`
	Protocol      uint   `gorm:"not null"`
	CreatedAt     time.Time
}

// ClusterInvitation contains no bearer secret. Consumption and the associated
// admission are committed together; old records remain to reject replay.
type ClusterInvitation struct {
	TokenHash string `gorm:"primaryKey;size:64"`
	ClusterID string `gorm:"size:32;not null;index"`
	IssuedBy  uint   `gorm:"not null"`
	// An administrator may authorize a second certificate hostname for the
	// invited member. This is not the member-published endpoint directory.
	AuthorizedEnrollmentOrigin string `gorm:"size:512;not null;default:''"`
	CreatedAt                  time.Time
	ExpiresAt                  time.Time `gorm:"not null"`
	ConsumedAt                 *time.Time
}

// ClusterAdmission records permission, not the live Raft role or readiness.
// A caller must verify possession of the bound public key before resuming.
// Revocation is retained; writing this row alone does not revoke transport TLS.
type ClusterAdmission struct {
	KeyFingerprint string `gorm:"primaryKey;size:64"`
	// Empty only for the unique seed admitted by an explicit offline operation.
	InvitationHash string `gorm:"size:64;not null;uniqueIndex"`
	Address        string `gorm:"size:255;not null;uniqueIndex:idx_cluster_admission_address,where:revoked_at IS NULL"`
	// Empty until the member proves its key and publishes a public HTTPS
	// endpoint. This is independent of the database transport address.
	EnrollmentOrigin string `gorm:"size:512;not null;default:''"`
	AdmittedAt       time.Time
	RevokedAt        *time.Time
	// MaterialGrantSequence forces each private-material authorization to
	// commit a distinct replicated write. It is not delivery or readiness.
	MaterialGrantSequence int64 `gorm:"not null;default:0;check:cluster_material_grant_sequence,material_grant_sequence >= 0"`
}

// ActionGrantRosterRecord retains each replicated roster revision. A database
// row is not an agent trust anchor; agents must pin and authenticate the roster
// independently before using its members to verify action grants.
type ActionGrantRosterRecord struct {
	ClusterID      string `gorm:"primaryKey;size:32;not null"`
	Revision       int64  `gorm:"primaryKey;not null;check:action_grant_roster_revision,revision > 0"`
	NotAfterUnixMS int64  `gorm:"not null;check:action_grant_roster_expiry,not_after_unix_ms > 0"`
	CreatedAt      time.Time
}

// ActionGrantRosterMemberRecord stores the exact Ed25519 verification key for
// a member in one roster revision. A transport certificate fingerprint alone
// cannot reconstruct these bytes.
type ActionGrantRosterMemberRecord struct {
	ClusterID string `gorm:"primaryKey;size:32;not null"`
	Revision  int64  `gorm:"primaryKey;not null"`
	// Raft IDs span uint64. SQLite stores the same 64 bits as a signed INTEGER;
	// casting back to uint64 restores IDs with the high bit set.
	MemberID  int64  `gorm:"primaryKey;not null;check:action_grant_roster_member_id,member_id <> 0"`
	Role      string `gorm:"size:16;not null"`
	KeyID     string `gorm:"size:128;not null"`
	PublicKey []byte `gorm:"not null"`
	Revoked   bool   `gorm:"not null"`
}

// ActionGrantReservation records one majority-committed issuer decision.
// The maximum old-authority expiry is derived by MAX(not_after_unix_ms) for a
// scheduling generation; no separately mutable lease counter is needed.
type ActionGrantReservation struct {
	GrantID              string `gorm:"primaryKey;size:128"`
	ClusterID            string `gorm:"size:32;not null"`
	SchedulingGeneration uint64 `gorm:"not null;index:idx_action_grant_generation_expiry"`
	NodeUUID             string `gorm:"size:64;not null;default:'';index;index:idx_action_grant_conflict,priority:1"`
	ActionFamily         string `gorm:"size:64;not null;default:''"`
	ConflictResourceID   string `gorm:"size:512;not null;default:'';index:idx_action_grant_conflict,priority:2"`
	OwnerMemberID        uint64 `gorm:"not null;default:0;serializer:raft_member_id"`
	SessionGeneration    uint64 `gorm:"not null;default:0;serializer:raft_member_id"`
	SessionUUID          string `gorm:"size:128;not null;default:''"`
	ConnectionID         string `gorm:"size:128;not null;default:''"`
	RosterRevision       uint64 `gorm:"not null"`
	NotAfterUnixMS       int64  `gorm:"not null;index:idx_action_grant_generation_expiry"`
	ClaimsSHA256         string `gorm:"size:64;not null"`
	CreatedAt            time.Time
}

// ActionDrainBarrier records that all reservations from prior generations
// expired and their bound agent sessions reported no unfinished calls. One
// row per cluster makes the hot reservation path a single replicated lookup.
type ActionDrainBarrier struct {
	ClusterID         string `gorm:"primaryKey;size:32"`
	ThroughGeneration uint64 `gorm:"not null"`
	ConfirmedAt       time.Time
}

// ActionRosterDrainBarrier records the old roster revisions whose grants
// expired and whose bound agent ledgers reported no unfinished calls.
type ActionRosterDrainBarrier struct {
	ClusterID       string `gorm:"primaryKey;size:32"`
	ThroughRevision uint64 `gorm:"not null"`
	ConfirmedAt     time.Time
}
