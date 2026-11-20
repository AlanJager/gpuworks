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

func registerEnrollmentTransportPolicy(router chi.Router, bundle *enrollmentBundle, st *store.Store) {
	router.Post(enrollment.TransportPolicyPath, authorizeEnrollmentMaterial(bundle, st, func(w http.ResponseWriter, r *http.Request) {
		// The outer wrapper proves TLS possession and checks the advertised address.
		// This second grant binds a fresh quorum write AND the versioned complete
		// snapshot to one commit, so a racing revocation cannot reuse that first grant.
		key, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
		revision, admissions, err := st.GrantClusterTransportAdmissions(r.Context(), expected, key)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterAdmissionRejected) {
				status = http.StatusForbidden
			}
			http.Error(w, "transport authorization snapshot unavailable", status)
			return
		}
		result := enrollment.TransportPolicySnapshot{ClusterID: expected.ClusterID, CAFingerprint: expected.CAFingerprint, Protocol: expected.Protocol, SourceRevision: revision, AllowedKeys: []string{}, RevokedKeys: []string{}}
		for _, admission := range admissions {
			if admission.RevokedAt != nil {
				result.RevokedKeys = append(result.RevokedKeys, admission.KeyFingerprint)
			} else {
				result.AllowedKeys = append(result.AllowedKeys, admission.KeyFingerprint)
			}
		}
		if revision == 0 || len(result.AllowedKeys) == 0 {
			http.Error(w, "transport authorization snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}))
}
