package proto

import (
	"bytes"
	"cmp"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"time"
)

const actionGrantVoteDomain = "gpuworks.action-grant-vote.v1\x00"
const actionRosterTransitionDomain = "gpuworks.action-roster-transition.v1\x00"
const maxActionRosterMembers = 512
const maxActionRosterInstallWindow = 10 * time.Minute

// TrustedActionRoster must come from an agent's locally pinned authority,
// never from the action frame being verified. NotAfterUnixMS is an absolute
// upper bound that an old roster cannot extend by signing another frame.
type TrustedActionRoster struct {
	ClusterID      string
	Revision       uint64
	NotAfterUnixMS int64
	Members        []TrustedActionMember
}

type TrustedActionMember struct {
	ID        uint64
	Role      string
	KeyID     string
	PublicKey ed25519.PublicKey
	Revoked   bool
}

type ActionGrantVote struct {
	MemberID  uint64 `json:"member_id"`
	Signature []byte `json:"signature"`
}

// ActionGrantQuorumProof is carried with the grant, but cannot define its own
// trust roster. The agent must match both values to its locally pinned roster.
type ActionGrantQuorumProof struct {
	RosterRevision       uint64            `json:"roster_revision"`
	RosterNotAfterUnixMS int64             `json:"roster_not_after_unix_ms"`
	Votes                []ActionGrantVote `json:"votes"`
}

// ActionRosterTransition binds one pinned roster to its exact successor.
// The deadline permits an offline agent to install a certificate after the
// old roster expires, without granting the old voters an unbounded extension.
type ActionRosterTransition struct {
	OldDigest           []byte            `json:"old_digest"`
	NewDigest           []byte            `json:"new_digest"`
	IssuedAtUnixMS      int64             `json:"issued_at_unix_ms"`
	InstallBeforeUnixMS int64             `json:"install_before_unix_ms"`
	Votes               []ActionGrantVote `json:"votes"`
}

// NewActionRosterTransition constructs the exact bytes that old voters sign.
// Each voter must also establish live membership and a healthy local clock.
func NewActionRosterTransition(old, next TrustedActionRoster, now time.Time) (ActionRosterTransition, error) {
	var empty ActionRosterTransition
	if err := ValidateTrustedActionRoster(old, now); err != nil {
		return empty, err
	}
	if err := ValidateTrustedActionRoster(next, now); err != nil {
		return empty, err
	}
	if old.ClusterID != next.ClusterID || next.Revision != old.Revision+1 {
		return empty, errors.New("action roster successor identity or revision mismatch")
	}
	deadline := now.Add(maxActionRosterInstallWindow).UnixMilli()
	if limit := actionRosterInstallLimit(old.NotAfterUnixMS); deadline > limit {
		deadline = limit
	}
	if deadline > next.NotAfterUnixMS {
		deadline = next.NotAfterUnixMS
	}
	if deadline <= now.UnixMilli() {
		return empty, errors.New("action roster successor has no install window")
	}
	oldDigest := sha256.Sum256(actionRosterBytes(old))
	newDigest := sha256.Sum256(actionRosterBytes(next))
	return ActionRosterTransition{OldDigest: oldDigest[:], NewDigest: newDigest[:], IssuedAtUnixMS: now.UnixMilli(), InstallBeforeUnixMS: deadline}, nil
}

// SignActionRosterTransitionVote signs one transition with a pinned old voter.
func SignActionRosterTransitionVote(old, next TrustedActionRoster, transition ActionRosterTransition, memberID uint64, key ed25519.PrivateKey, now time.Time) (ActionGrantVote, error) {
	if err := validateActionRosterTransition(old, next, transition, now, true); err != nil {
		return ActionGrantVote{}, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return ActionGrantVote{}, errors.New("invalid action roster transition key")
	}
	for _, member := range old.Members {
		if member.ID == memberID && member.Role == "voter" && !member.Revoked && bytes.Equal(member.PublicKey, key.Public().(ed25519.PublicKey)) {
			return ActionGrantVote{MemberID: memberID, Signature: ed25519.Sign(key, actionRosterTransitionVoteBytes(transition, memberID))}, nil
		}
	}
	return ActionGrantVote{}, errors.New("action roster transition signer is not an active old voter")
}

// VerifyActionRosterTransition accepts only a majority of the locally pinned
// old roster. The signed deadline remains usable after that roster's expiry.
func VerifyActionRosterTransition(old, next TrustedActionRoster, transition ActionRosterTransition, now time.Time) error {
	if err := validateActionRosterTransition(old, next, transition, now, false); err != nil {
		return err
	}
	issued := time.UnixMilli(transition.IssuedAtUnixMS)
	members, voters, err := validatedActionRoster(old, issued)
	if err != nil {
		return err
	}
	if len(transition.Votes) > len(old.Members) {
		return errors.New("too many action roster transition votes")
	}
	seen := make(map[uint64]bool, len(transition.Votes))
	for _, vote := range transition.Votes {
		member := members[vote.MemberID]
		if member.ID == 0 || member.Role != "voter" || member.Revoked || seen[vote.MemberID] || len(vote.Signature) != ed25519.SignatureSize ||
			!ed25519.Verify(member.PublicKey, actionRosterTransitionVoteBytes(transition, vote.MemberID), vote.Signature) {
			return errors.New("invalid action roster transition vote")
		}
		seen[vote.MemberID] = true
	}
	if len(seen) <= voters/2 {
		return errors.New("action roster transition lacks old voter majority")
	}
	return nil
}

// VerifyActionRosterTransitionVote checks one response before a collector adds
// it to a quorum. It does not establish the majority on its own.
func VerifyActionRosterTransitionVote(old, next TrustedActionRoster, transition ActionRosterTransition, vote ActionGrantVote, now time.Time) error {
	if err := validateActionRosterTransition(old, next, transition, now, false); err != nil {
		return err
	}
	members, _, err := validatedActionRoster(old, time.UnixMilli(transition.IssuedAtUnixMS))
	if err != nil {
		return err
	}
	member := members[vote.MemberID]
	if member.ID == 0 || member.Role != "voter" || member.Revoked || len(vote.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(member.PublicKey, actionRosterTransitionVoteBytes(transition, vote.MemberID), vote.Signature) {
		return errors.New("invalid action roster transition vote")
	}
	return nil
}

func validateActionRosterTransition(old, next TrustedActionRoster, transition ActionRosterTransition, now time.Time, signing bool) error {
	if old.ClusterID != next.ClusterID || next.Revision != old.Revision+1 || transition.IssuedAtUnixMS <= 0 ||
		transition.InstallBeforeUnixMS <= transition.IssuedAtUnixMS ||
		transition.InstallBeforeUnixMS > actionRosterInstallLimit(transition.IssuedAtUnixMS) ||
		transition.InstallBeforeUnixMS > actionRosterInstallLimit(old.NotAfterUnixMS) ||
		transition.InstallBeforeUnixMS > next.NotAfterUnixMS ||
		!now.Before(time.UnixMilli(transition.InstallBeforeUnixMS)) ||
		transition.IssuedAtUnixMS > now.Add(ActionGrantMaxClockSkew).UnixMilli() {
		return errors.New("invalid action roster transition lifetime or identity")
	}
	oldDigest := sha256.Sum256(actionRosterBytes(old))
	newDigest := sha256.Sum256(actionRosterBytes(next))
	if !bytes.Equal(transition.OldDigest, oldDigest[:]) || !bytes.Equal(transition.NewDigest, newDigest[:]) {
		return errors.New("action roster transition digest mismatch")
	}
	if err := ValidateTrustedActionRoster(next, now); err != nil {
		return err
	}
	if signing {
		if now.UnixMilli() < transition.IssuedAtUnixMS || !now.Before(time.UnixMilli(old.NotAfterUnixMS)) {
			return errors.New("old action roster cannot sign transition now")
		}
		return ValidateTrustedActionRoster(old, now)
	}
	return nil
}

func actionRosterInstallLimit(fromUnixMS int64) int64 {
	window := maxActionRosterInstallWindow.Milliseconds()
	if fromUnixMS > math.MaxInt64-window {
		return math.MaxInt64
	}
	return fromUnixMS + window
}

func actionRosterTransitionVoteBytes(transition ActionRosterTransition, memberID uint64) []byte {
	b := []byte(actionRosterTransitionDomain)
	b = append(b, transition.OldDigest...)
	b = append(b, transition.NewDigest...)
	b = binary.BigEndian.AppendUint64(b, uint64(transition.IssuedAtUnixMS))
	b = binary.BigEndian.AppendUint64(b, uint64(transition.InstallBeforeUnixMS))
	return binary.BigEndian.AppendUint64(b, memberID)
}

// SignActionGrantVote only creates one vote. The caller must independently
// establish the member's current authority before signing a live action.
func SignActionGrantVote(grant ActionGrant, roster TrustedActionRoster, memberID uint64, privateKey ed25519.PrivateKey) (ActionGrantVote, error) {
	if err := validateActionGrantClaims(grant.Claims); err != nil {
		return ActionGrantVote{}, err
	}
	if memberID == 0 || len(privateKey) != ed25519.PrivateKeySize || roster.Revision == 0 || roster.NotAfterUnixMS <= 0 ||
		len(roster.Members) == 0 || len(roster.Members) > maxActionRosterMembers {
		return ActionGrantVote{}, errors.New("invalid action grant voter or roster")
	}
	if grant.Claims.ClusterID != roster.ClusterID || grant.Claims.NotAfterUnixMS > roster.NotAfterUnixMS {
		return ActionGrantVote{}, errors.New("action grant exceeds voter roster")
	}
	matched := false
	for _, member := range roster.Members {
		if member.ID == memberID && member.Role == "voter" && !member.Revoked &&
			bytes.Equal(member.PublicKey, privateKey.Public().(ed25519.PublicKey)) {
			matched = true
			break
		}
	}
	if !matched {
		return ActionGrantVote{}, errors.New("action grant signer is not a roster voter")
	}
	return ActionGrantVote{MemberID: memberID, Signature: ed25519.Sign(privateKey, actionGrantVoteBytes(grant, roster, memberID))}, nil
}

// ValidateTrustedActionRoster checks bytes supplied by a separate, trusted
// installation channel before an agent pins them locally. Structural validity
// alone does not authenticate the source or authorize a roster transition.
func ValidateTrustedActionRoster(roster TrustedActionRoster, now time.Time) error {
	_, _, err := validatedActionRoster(roster, now)
	return err
}

func validatedActionRoster(roster TrustedActionRoster, now time.Time) (map[uint64]TrustedActionMember, int, error) {
	if !validActionGrantText(roster.ClusterID) || roster.Revision == 0 || roster.NotAfterUnixMS <= 0 ||
		len(roster.Members) == 0 || len(roster.Members) > maxActionRosterMembers {
		return nil, 0, errors.New("invalid action grant roster")
	}
	if !now.Before(time.UnixMilli(roster.NotAfterUnixMS)) {
		return nil, 0, errors.New("action grant roster expired")
	}
	members := make(map[uint64]TrustedActionMember, len(roster.Members))
	keys := make(map[string]bool, len(roster.Members))
	keyIDs := make(map[string]bool, len(roster.Members))
	voters := 0
	activeVoters := 0
	for _, member := range roster.Members {
		if member.ID == 0 || !validActionGrantText(member.KeyID) || len(member.PublicKey) != ed25519.PublicKeySize ||
			members[member.ID].ID != 0 || keys[string(member.PublicKey)] || keyIDs[member.KeyID] {
			return nil, 0, errors.New("invalid action grant roster member")
		}
		if member.Role != "voter" && member.Role != "spare" && member.Role != "stand-by" {
			return nil, 0, errors.New("invalid action grant roster role")
		}
		members[member.ID] = member
		keys[string(member.PublicKey)] = true
		keyIDs[member.KeyID] = true
		// Revocation cannot shrink the majority denominator within a pinned
		// revision. A smaller voter set needs a newly certified roster.
		if member.Role == "voter" {
			voters++
			if !member.Revoked {
				activeVoters++
			}
		}
	}
	if activeVoters == 0 {
		return nil, 0, errors.New("action grant roster has no active voter")
	}
	return members, voters, nil
}

// VerifyActionGrantWithQuorum checks one exact action against an independently
// pinned roster and a majority of its active voters. It does not authenticate
// how that roster was installed or authorize a successor roster.
func VerifyActionGrantWithQuorum(grant ActionGrant, proof ActionGrantQuorumProof, roster TrustedActionRoster, expected ActionGrantScope, now time.Time) error {
	if grant.Claims.ClusterID != roster.ClusterID || proof.RosterRevision != roster.Revision ||
		proof.RosterNotAfterUnixMS != roster.NotAfterUnixMS {
		return errors.New("action grant roster binding mismatch")
	}
	if len(proof.Votes) > len(roster.Members) {
		return errors.New("invalid action grant roster or vote count")
	}
	if grant.Claims.NotAfterUnixMS > roster.NotAfterUnixMS {
		return errors.New("action grant roster expired")
	}
	members, voters, err := validatedActionRoster(roster, now)
	if err != nil {
		return err
	}
	issuer := members[grant.Claims.IssuerMemberID]
	if issuer.ID == 0 || issuer.Role != "voter" || issuer.Revoked {
		return errors.New("action grant issuer is not an active voter")
	}
	if err := VerifyActionGrant(grant, issuer.PublicKey, issuer.KeyID, expected, now); err != nil {
		return err
	}
	seen := make(map[uint64]bool, len(proof.Votes))
	valid := 0
	for _, vote := range proof.Votes {
		member := members[vote.MemberID]
		if member.ID == 0 || member.Role != "voter" || member.Revoked || seen[vote.MemberID] || len(vote.Signature) != ed25519.SignatureSize ||
			!ed25519.Verify(member.PublicKey, actionGrantVoteBytes(grant, roster, vote.MemberID), vote.Signature) {
			return errors.New("invalid action grant quorum vote")
		}
		seen[vote.MemberID] = true
		valid++
	}
	if valid <= voters/2 {
		return errors.New("action grant lacks voter majority")
	}
	return nil
}

// VerifyActionGrantVote checks one returned voter signature against an
// independently obtained roster. It does not establish a majority by itself.
func VerifyActionGrantVote(grant ActionGrant, roster TrustedActionRoster, vote ActionGrantVote, now time.Time) error {
	if grant.Claims.ClusterID != roster.ClusterID || grant.Claims.NotAfterUnixMS > roster.NotAfterUnixMS ||
		!now.Before(time.UnixMilli(grant.Claims.NotAfterUnixMS)) {
		return errors.New("action grant vote roster or lifetime mismatch")
	}
	members, _, err := validatedActionRoster(roster, now)
	if err != nil {
		return err
	}
	member := members[vote.MemberID]
	if member.ID == 0 || member.Role != "voter" || member.Revoked || len(vote.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(member.PublicKey, actionGrantVoteBytes(grant, roster, vote.MemberID), vote.Signature) {
		return errors.New("invalid action grant vote")
	}
	return nil
}

func actionGrantVoteBytes(grant ActionGrant, roster TrustedActionRoster, memberID uint64) []byte {
	var b bytes.Buffer
	b.WriteString(actionGrantVoteDomain)
	rosterDigest := sha256.Sum256(actionRosterBytes(roster))
	b.Write(rosterDigest[:])
	b.Write(binary.BigEndian.AppendUint64(nil, memberID))
	b.Write(actionGrantSigningBytes(grant.Claims))
	b.Write(grant.Signature)
	return b.Bytes()
}

func actionRosterBytes(roster TrustedActionRoster) []byte {
	b := binary.AppendUvarint(nil, uint64(len(roster.ClusterID)))
	b = append(b, roster.ClusterID...)
	b = binary.BigEndian.AppendUint64(b, roster.Revision)
	b = binary.BigEndian.AppendUint64(b, uint64(roster.NotAfterUnixMS))
	members := slices.Clone(roster.Members)
	slices.SortFunc(members, func(a, c TrustedActionMember) int { return cmp.Compare(a.ID, c.ID) })
	b = binary.AppendUvarint(b, uint64(len(members)))
	for _, member := range members {
		b = binary.BigEndian.AppendUint64(b, member.ID)
		for _, value := range []string{member.Role, member.KeyID} {
			b = binary.AppendUvarint(b, uint64(len(value)))
			b = append(b, value...)
		}
		b = binary.AppendUvarint(b, uint64(len(member.PublicKey)))
		b = append(b, member.PublicKey...)
		if member.Revoked {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
	}
	return b
}
