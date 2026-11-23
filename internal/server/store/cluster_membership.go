package store

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

type ClusterTransportMember struct {
	ID               uint64
	Address          string
	Role             string
	KeyFingerprint   string
	EnrollmentOrigin string
}

type clusterMembershipGrant func(context.Context, ClusterIdentity, string) (uint64, []ClusterTransportMember, error)

const ClusterMembershipGrantTimeout = 5 * time.Second

// GrantClusterMembership observes the live configuration before and after a
// majority-confirmed authorization grant. Matching observations are not an
// atomic configuration freeze or a readiness guarantee for pending members.
func (s *Store) GrantClusterMembership(ctx context.Context, expected ClusterIdentity, requesterKey string) (uint64, []ClusterTransportMember, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ClusterMembershipGrantTimeout)
	defer cancel()
	if s.grantClusterMembership == nil {
		return 0, nil, errors.New("live native cluster membership is unavailable")
	}
	revision, members, err := s.grantClusterMembership(ctx, expected, requesterKey)
	s.rememberConfirmedMembership(members, err)
	return revision, members, err
}

// GrantClusterMembershipKeys increments every named admission in one replicated
// transaction, between one pair of live membership observations. Callers use it
// to bracket a multi-source proof without a commit per source.
func (s *Store) GrantClusterMembershipKeys(ctx context.Context, expected ClusterIdentity, keys []string) (uint64, []ClusterTransportMember, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ClusterMembershipGrantTimeout)
	defer cancel()
	if s.grantClusterMembershipKeys == nil {
		return 0, nil, errors.New("live native cluster membership is unavailable")
	}
	revision, members, err := s.grantClusterMembershipKeys(ctx, expected, keys)
	s.rememberConfirmedMembership(members, err)
	return revision, members, err
}

// elapsedClock reads the clock for measuring elapsed time in memory. Its value
// keeps the monotonic reading, so a step of the wall clock neither shortens
// nor stretches an age measured with it; time.Now().UTC() drops that reading.
// A value from it is never written to the database or serialized, which is
// what the UTC rule for this package is about.
var elapsedClock = time.Now

// confirmedClusterMembership is the member list of the latest successful
// grant, kept in memory only.
type confirmedClusterMembership struct {
	members []ClusterTransportMember
	at      time.Time
}

func (s *Store) rememberConfirmedMembership(members []ClusterTransportMember, err error) {
	if err != nil || len(members) == 0 {
		return
	}
	s.confirmedMembership.Store(&confirmedClusterMembership{members: slices.Clone(members), at: elapsedClock()})
}

// ConfirmedClusterMembership returns the member list the latest successful
// grant of this process confirmed, when that grant is no older than maxAge.
// It reads memory only. Callers that must not wait for a replicated write use
// it between grants: a member removed since then stays in the list, and one
// added since then is missing, for at most maxAge.
// ForgetConfirmedClusterMembership drops that list, so the next caller takes a
// fresh grant. The member calls it when its own admission is invalidated.
func (s *Store) ForgetConfirmedClusterMembership() {
	s.confirmedMembership.Store(nil)
}

func (s *Store) ConfirmedClusterMembership(maxAge time.Duration) ([]ClusterTransportMember, bool) {
	confirmed := s.confirmedMembership.Load()
	if confirmed == nil || maxAge <= 0 || time.Since(confirmed.at) > maxAge {
		return nil, false
	}
	return slices.Clone(confirmed.members), true
}

func normalizeLiveClusterMembers(input []cluster.Member) ([]cluster.Member, error) {
	if len(input) == 0 || len(input) > 512 {
		return nil, errors.New("live cluster membership is empty or exceeds capacity")
	}
	members := slices.Clone(input)
	ids := make(map[uint64]bool, len(input))
	addresses := make(map[string]bool, len(input))
	for i := range members {
		member := &members[i]
		if member.ID == 0 || ids[member.ID] {
			return nil, errors.New("live cluster member has invalid or duplicate identity")
		}
		switch member.Role {
		case "voter", "stand-by", "spare":
		default:
			return nil, errors.New("live cluster member has invalid role")
		}
		address, err := normalizeClusterMemberAddress(member.Address)
		if err != nil {
			return nil, err
		}
		if addresses[address] {
			return nil, errors.New("live cluster member has duplicate address")
		}
		member.Address = address
		ids[member.ID] = true
		addresses[address] = true
	}
	slices.SortFunc(members, func(a, b cluster.Member) int { return cmp.Compare(a.ID, b.ID) })
	return members, nil
}

func bindLiveClusterAdmissions(live []cluster.Member, admissions []ClusterAdmission) ([]ClusterTransportMember, error) {
	active := make(map[string]ClusterAdmission, len(admissions))
	keys := make(map[string]bool, len(admissions))
	for _, admission := range admissions {
		if admission.RevokedAt != nil {
			continue
		}
		address, err := normalizeClusterMemberAddress(admission.Address)
		if err != nil || address != admission.Address || !validClusterDigest(admission.KeyFingerprint, 32) {
			return nil, ErrClusterAdmissionRejected
		}
		if admission.EnrollmentOrigin != "" {
			origin, err := normalizeClusterEnrollmentOrigin(admission.EnrollmentOrigin)
			if err != nil || origin != admission.EnrollmentOrigin {
				return nil, ErrClusterEnrollmentOriginInvalid
			}
		}
		if _, exists := active[address]; exists || keys[admission.KeyFingerprint] {
			return nil, errors.New("active cluster admissions have duplicate key or address")
		}
		active[address] = admission
		keys[admission.KeyFingerprint] = true
	}
	result := make([]ClusterTransportMember, 0, len(live))
	boundKeys := make(map[string]bool, len(live))
	for _, member := range live {
		admission, exists := active[member.Address]
		if !exists || boundKeys[admission.KeyFingerprint] {
			return nil, errors.New("live cluster member has no unique active admission")
		}
		boundKeys[admission.KeyFingerprint] = true
		result = append(result, ClusterTransportMember{ID: member.ID, Address: member.Address, Role: member.Role, KeyFingerprint: admission.KeyFingerprint, EnrollmentOrigin: admission.EnrollmentOrigin})
	}
	return result, nil
}
