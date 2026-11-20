//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Bootstrap requests have no client identity yet. Private member requests must
// prove possession on this same TLS connection; proxy headers are never proof.
func enrollmentClientTLS(config *tls.Config, ca []byte) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid enrollment client authority")
	}
	result := config.Clone()
	result.ClientAuth = tls.VerifyClientCertIfGiven
	result.ClientCAs = roots
	result.SessionTicketsDisabled = true
	return result, nil
}

func enrollmentMaterialHandler(bundle *enrollmentBundle, st *store.Store, public http.Handler, catalog *packages.Store) http.Handler {
	return enrollmentMaterialHandlerWithReceiver(bundle, st, public, catalog, nil)
}

func enrollmentMaterialHandlerWithReceiver(bundle *enrollmentBundle, st *store.Store, public http.Handler, catalog *packages.Store, receiver *materialReceiver) http.Handler {
	return enrollmentMaterialHandlerWithTrackedReceiver(bundle, st, public, catalog, receiver, nil)
}

func enrollmentMaterialHandlerWithTrackedReceiver(bundle *enrollmentBundle, st *store.Store, public http.Handler, catalog *packages.Store, receiver *materialReceiver, requests *materialRequestTracker) http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	})
	router.Post(enrollment.SecretsPath, authorizeEnrollmentMaterial(bundle, st, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The validated startup snapshot also drives application encryption.
		// Do not reread mutable files after authorization or substitute new keys.
		_ = json.NewEncoder(w).Encode(enrollment.BootstrapSecrets{
			ClusterID: bundle.info.ClusterID, Protocol: bundle.info.Protocol,
			CACertificatePEM: string(bundle.ca), CAKeyPEM: string(bundle.caKey), MasterSecret: []byte(bundle.master),
		})
	}))
	registerEnrollmentCatalog(router, bundle, st, catalog)
	if receiver != nil {
		if requests == nil {
			requests = newMaterialRequestTracker()
		}
		registerEnrollmentMaterialReceiver(router, bundle, st, receiver, requests)
	}
	registerEnrollmentTransportPolicy(router, bundle, st)
	registerEnrollmentTransportAuthorization(router, bundle, st)
	registerEnrollmentMembership(router, bundle, st)
	registerEnrollmentOrigin(router, bundle, st)
	router.Mount("/", public)
	return router
}

func authorizeEnrollmentMaterial(bundle *enrollmentBundle, st *store.Store, next http.HandlerFunc) http.HandlerFunc {
	return authorizeEnrollmentMember(bundle, st, true, next)
}

// materialAuthorizationGrantTimeout fails an unavailable grant while the
// caller's hedged backup still has its own batch budget. The grant itself
// remains a fresh majority-confirmed write; only the wait is shortened.
const materialAuthorizationGrantTimeout = 1500 * time.Millisecond

func authorizeEnrollmentMember(bundle *enrollmentBundle, st *store.Store, emptyBody bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		if emptyBody {
			// Bootstrap reads take no body, bearer token, CSR, or claimed fingerprint.
			body, err := io.ReadAll(io.LimitReader(r.Body, 1))
			if err != nil || len(body) != 0 {
				http.Error(w, "material request must be empty", http.StatusBadRequest)
				return
			}
		}
		grantCtx, cancelGrant := context.WithTimeout(r.Context(), materialAuthorizationGrantTimeout)
		grant, err := st.GrantClusterMaterials(grantCtx, bundle.info.ClusterID, key)
		cancelGrant()
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterAdmissionRejected) {
				status = http.StatusForbidden
			} else {
				// No bytes were read: the sender must not take this as a verdict.
				w.Header().Set(materialRefusalHeader, materialRefusalAuthorization)
			}
			http.Error(w, "material authorization unavailable", status)
			return
		}
		host, _, err := net.SplitHostPort(grant.Address)
		if err != nil || leaf.VerifyHostname(host) != nil {
			http.Error(w, "member address rejected", http.StatusForbidden)
			return
		}
		if r.Context().Err() != nil {
			return
		}

		next(w, r)
	}
}
