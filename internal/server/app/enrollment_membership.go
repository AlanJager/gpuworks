//go:build linux && dqlite

package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func registerEnrollmentMembership(router chi.Router, bundle *enrollmentBundle, st *store.Store) {
	router.Post(enrollment.ClusterMembershipPath, authorizeEnrollmentMaterial(bundle, st, func(w http.ResponseWriter, r *http.Request) {
		requester, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
		revision, members, err := st.GrantClusterMembership(r.Context(), expected, requester)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterAdmissionRejected) {
				status = http.StatusForbidden
			}
			http.Error(w, "cluster membership unavailable", status)
			return
		}
		result := enrollment.ClusterMembershipSnapshot{ClusterID: expected.ClusterID, CAFingerprint: expected.CAFingerprint, Protocol: expected.Protocol, SourceRevision: revision, Members: make([]enrollment.ClusterMember, 0, len(members))}
		for _, member := range members {
			result.Members = append(result.Members, enrollment.ClusterMember{ID: member.ID, Address: member.Address, Role: member.Role, KeyFingerprint: member.KeyFingerprint, EnrollmentOrigin: member.EnrollmentOrigin})
		}
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	}))
}
