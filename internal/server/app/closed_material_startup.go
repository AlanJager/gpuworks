//go:build linux && dqlite

package app

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The facts listener precedes Store classification and receiver construction.
// Known material routes must deny temporarily rather than advertise a missing
// route. Discovery authenticates this denial; it grants no material authority.
func registerClosedMaterialUnavailable(router chi.Router, bundle *enrollmentBundle, session joinedRaftSession, requests *materialRequestTracker) {
	reader, supported := session.(interface {
		ReadQuorumDiscoveryForCluster(context.Context, store.ClusterIdentity, string) (store.ClusterAdmission, []store.ClusterTransportMember, error)
	})
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	deny := requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if digest := chi.URLParam(r, "digest"); digest != "" && !validMaterialDigest(digest) {
			http.NotFound(w, r)
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "invalid material request", http.StatusBadRequest)
			return
		}
		if r.TLS == nil || !r.TLS.HandshakeComplete || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "member authentication required", http.StatusUnauthorized)
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		key, err := bundle.trust.VerifyMember(leaf, time.Now().UTC())
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		if !supported {
			refuseUnstartedMaterialReceiver(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), store.ClusterMembershipGrantTimeout)
		defer cancel()
		admission, members, err := reader.ReadQuorumDiscoveryForCluster(ctx, expected, key)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterAdmissionRejected) || errors.Is(err, store.ErrClusterIdentityConflict) {
				status = http.StatusForbidden
			}
			if status == http.StatusServiceUnavailable {
				w.Header().Set(materialRefusalHeader, materialRefusalNotStarted)
			}
			http.Error(w, "material receiver unavailable", status)
			return
		}
		if admission.KeyFingerprint != key || admission.RevokedAt != nil || !factsCertificateCovers(leaf, admission.Address) {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		provider, err := x509.ParseCertificate(bundle.tls.Certificates[0].Certificate[0])
		if err != nil {
			refuseUnstartedMaterialReceiver(w)
			return
		}
		providerKey, err := bundle.trust.VerifyMember(provider, time.Now().UTC())
		if err != nil {
			refuseUnstartedMaterialReceiver(w)
			return
		}
		matches := 0
		for _, member := range members {
			if member.KeyFingerprint == providerKey && factsCertificateCovers(provider, member.Address) {
				matches++
			}
		}
		if matches != 1 {
			http.Error(w, "member identity rejected", http.StatusForbidden)
			return
		}
		refuseUnstartedMaterialReceiver(w)
	})
	router.Post(materialBatchPath, deny)
	for _, method := range []string{http.MethodHead, http.MethodGet, http.MethodPost} {
		router.Method(method, materialReplicaPath+"/{digest}", deny)
	}
	for _, action := range []string{"verify", "materialize", "installed"} {
		router.Post(materialManifestPath+"/{digest}/"+action, deny)
	}
}

// refuseUnstartedMaterialReceiver answers an authenticated member while this
// member is closed: there is no receiver, so no byte was inspected, and the
// label lets the sender treat this member as unavailable instead of as a
// verdict on its material.
func refuseUnstartedMaterialReceiver(w http.ResponseWriter) {
	w.Header().Set(materialRefusalHeader, materialRefusalNotStarted)
	http.Error(w, "material receiver unavailable", http.StatusServiceUnavailable)
}
