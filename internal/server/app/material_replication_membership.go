//go:build linux && dqlite

package app

import "gpuworks.ai/gpuworks/internal/server/store"

// A peer restart changes the live member list. That must not close this
// member's business while it is still admitted. Close only when this process
// itself has disappeared from the list.
func localMaterialMemberAbsent(sourceID uint64, publicURL string, members []store.ClusterTransportMember) bool {
	for _, member := range members {
		if member.ID == sourceID && member.EnrollmentOrigin == publicURL {
			return false
		}
	}
	return true
}
