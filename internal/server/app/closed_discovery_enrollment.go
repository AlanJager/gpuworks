//go:build linux && dqlite

package app

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Discovery returns dial hints from the original runtime. It does not establish
// fresh quorum, process closure, or authorization to start another member.
func closedQuorumDiscoveryHandler(bundle *enrollmentBundle, session joinedRaftSession, requests *materialRequestTracker) (http.HandlerFunc, error) {
	if requests == nil || bundle == nil || bundle.trust == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 {
		return nil, errors.New("quorum discovery requires an authenticated enrollment identity")
	}
	pair := bundle.tls.Certificates[0]
	if len(pair.Certificate) == 0 {
		return nil, errors.New("quorum discovery requires the actual listener certificate")
	}
	providerCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	reader, supported := session.(quorumDiscoveryReader)
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	return requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		now := time.Now().UTC()
		requester, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], now)
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		provider, err := bundle.trust.VerifyMember(providerCert, now)
		if err != nil {
			http.Error(w, "quorum discovery provider unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Body == nil {
			http.Error(w, "invalid quorum discovery request", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			http.Error(w, "invalid quorum discovery request", http.StatusBadRequest)
			return
		}
		request, err := enrollment.DecodeClusterQuorumFactsRequest(data)
		if err != nil {
			http.Error(w, "invalid quorum discovery request", http.StatusBadRequest)
			return
		}
		if !supported {
			http.Error(w, "cluster quorum discovery unavailable", http.StatusServiceUnavailable)
			return
		}
		admission, members, err := reader.ReadQuorumDiscoveryForCluster(r.Context(), expected, requester)
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterAdmissionRejected) {
				status = http.StatusForbidden
			}
			http.Error(w, "cluster quorum discovery unavailable", status)
			return
		}
		if admission.KeyFingerprint != requester || admission.RevokedAt != nil || !factsCertificateCovers(r.TLS.PeerCertificates[0], admission.Address) {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		if len(members) == 0 || len(members) > 512 {
			http.Error(w, "cluster quorum discovery unavailable", http.StatusServiceUnavailable)
			return
		}
		discovery := enrollment.ClusterQuorumDiscovery{
			ClusterID: expected.ClusterID, CAFingerprint: expected.CAFingerprint, Protocol: expected.Protocol,
			Nonce: request.Nonce, Members: make([]enrollment.ClusterMember, 0, len(members)),
		}
		providerMatches := 0
		for _, member := range members {
			discovery.Members = append(discovery.Members, enrollment.ClusterMember{
				ID: member.ID, Address: member.Address, Role: member.Role,
				KeyFingerprint: member.KeyFingerprint, EnrollmentOrigin: member.EnrollmentOrigin,
			})
			if member.KeyFingerprint == provider {
				if !factsCertificateCovers(providerCert, member.Address) {
					http.Error(w, "quorum discovery provider unavailable", http.StatusServiceUnavailable)
					return
				}
				providerMatches++
			}
		}
		if providerMatches != 1 {
			http.Error(w, "quorum discovery provider unavailable", http.StatusServiceUnavailable)
			return
		}
		sort.Slice(discovery.Members, func(i, j int) bool { return discovery.Members[i].ID < discovery.Members[j].ID })
		data, err = json.Marshal(discovery)
		if err == nil {
			_, err = enrollment.DecodeClusterQuorumDiscovery(data)
		}
		if err != nil {
			http.Error(w, "cluster quorum discovery unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append(data, '\n'))
	}), nil
}
