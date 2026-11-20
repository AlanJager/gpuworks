//go:build linux && dqlite

package app

import (
	"errors"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Public origins are persisted admission facts, never inferred from Raft ports.
func sparseDiscoveryContacts(members []store.ClusterTransportMember, allowed []string) (map[string]string, error) {
	if len(members) == 0 || len(members) > 512 {
		return nil, errors.New("sparse discovery requires bounded public members")
	}
	contacts := make(map[string]string)
	for _, member := range members {
		index := sort.SearchStrings(allowed, member.KeyFingerprint)
		if index == len(allowed) || allowed[index] != member.KeyFingerprint {
			return nil, errors.New("sparse directory member is not allowed by transport policy")
		}
		if member.EnrollmentOrigin == "" {
			continue
		}
		if !enrollment.CanonicalEnrollmentOrigin(member.EnrollmentOrigin) {
			return nil, errors.New("sparse directory HTTPS origin is not canonical")
		}
		if previous, ok := contacts[member.EnrollmentOrigin]; ok && previous != member.KeyFingerprint {
			return nil, errors.New("sparse directory assigns different keys to one HTTPS origin")
		}
		contacts[member.EnrollmentOrigin] = member.KeyFingerprint
	}
	if len(contacts) == 0 {
		return nil, errors.New("sparse directory has no explicit HTTPS contact")
	}
	return contacts, nil
}
