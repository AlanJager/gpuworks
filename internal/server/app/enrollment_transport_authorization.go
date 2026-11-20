//go:build linux && dqlite

package app

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func registerEnrollmentTransportAuthorization(router chi.Router, bundle *enrollmentBundle, st *store.Store) {
	router.Post(enrollment.TransportAuthorizationPath, authorizeEnrollmentMaterial(bundle, st, func(w http.ResponseWriter, r *http.Request) {
		requester, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		if bundle.tls == nil || len(bundle.tls.Certificates) != 1 || len(bundle.tls.Certificates[0].Certificate) == 0 {
			http.Error(w, "transport authorization confirmation unavailable", http.StatusServiceUnavailable)
			return
		}
		localCertificate, err := x509.ParseCertificate(bundle.tls.Certificates[0].Certificate[0])
		if err != nil {
			http.Error(w, "transport authorization confirmation unavailable", http.StatusServiceUnavailable)
			return
		}
		localKey, err := bundle.trust.VerifyMember(localCertificate, time.Now().UTC())
		if err != nil {
			http.Error(w, "transport authorization confirmation unavailable", http.StatusServiceUnavailable)
			return
		}
		policy, err := st.ConfirmTransportAuthorization(r.Context(), requester, localKey)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterAdmissionRejected) {
				status = http.StatusForbidden
			}
			http.Error(w, "transport authorization confirmation unavailable", status)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(enrollment.TransportAuthorizationReceipt{
			ClusterID: policy.ClusterID, CAFingerprint: policy.CAFingerprint, Protocol: bundle.info.Protocol,
			MemberKey: localKey, SourceRevision: policy.SourceRevision, LocalRevision: policy.Revision,
		})
	}))
}
