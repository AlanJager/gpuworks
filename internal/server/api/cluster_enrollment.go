package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ClusterEnrollmentDeps are specific to the authenticated cluster bootstrap
// listener. They do not enable multi-member business startup in the main App.
type ClusterEnrollmentDeps struct {
	Store     *store.Store
	Signer    *auth.TokenSigner
	Issuer    ClusterCertificateIssuer
	PublicURL string
	// StorageTier is the cluster's recorded timing profile (empty for the
	// standard one), told to every member that is issued a certificate.
	StorageTier string
}

// ClusterCertificateIssuer owns signing policy and private material. The
// assembly layer loads it explicitly; HTTP handlers only see public outputs.
type ClusterCertificateIssuer interface {
	VerifyIdentity(clusterID, fingerprint string, now time.Time) ([]byte, error)
	CSRKeyFingerprint(csrPEM []byte) (string, error)
	IssueMemberWithAdditionalHost(csrPEM []byte, admittedKey, address, additionalHost string, now time.Time, lifetime time.Duration) ([]byte, error)
}

type clusterEnrollment struct {
	store          *store.Store
	signer         *auth.TokenSigner
	authority      ClusterCertificateIssuer
	identity       store.ClusterIdentity
	caPEM          string
	storageTier    string
	certificateURL string
}

// ClusterEnrollmentRouter exposes enrollment's public-certificate operations.
// It must be served over TLS. Reverse-proxy headers cannot supply that proof.
// No handler here returns cluster secrets or starts a database member.
func ClusterEnrollmentRouter(ctx context.Context, d ClusterEnrollmentDeps) (http.Handler, error) {
	if d.Store == nil || d.Signer == nil || d.Issuer == nil {
		return nil, errors.New("cluster enrollment requires store and management authentication")
	}
	endpoint, err := url.Parse(d.PublicURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, errors.New("cluster enrollment requires an explicit HTTPS origin")
	}
	cluster, err := d.Store.ClusterIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if cluster.Protocol != store.ClusterProtocolVersion {
		return nil, errors.New("unsupported cluster enrollment protocol")
	}
	caPEM, err := d.Issuer.VerifyIdentity(cluster.ClusterID, cluster.CAFingerprint, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	a := &clusterEnrollment{store: d.Store, signer: d.Signer, authority: d.Issuer, identity: cluster, caPEM: string(caPEM), storageTier: d.StorageTier, certificateURL: strings.TrimSuffix(d.PublicURL, "/") + "/api/v1/cluster/enrollment/certificate"}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if r.TLS == nil || !r.TLS.HandshakeComplete {
				writeProductError(w, http.StatusUpgradeRequired, ErrorClusterTLSRequired, "cluster enrollment requires HTTPS", nil)
				return
			}
			if r.URL.RawQuery != "" {
				writeProductError(w, http.StatusBadRequest, ErrorClusterRequestInvalid, "cluster credentials and parameters must not appear in the URL", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	r.With(managementAdmin(d.Store, d.Signer)).Post("/api/v1/cluster/invitations", a.invite)
	r.Post("/api/v1/cluster/enrollment/certificate", a.enroll)
	r.Post("/api/v1/cluster/enrollment/certificate/recover", a.recoverCertificate)
	return r, nil
}

func (a *clusterEnrollment) current(w http.ResponseWriter, r *http.Request) bool {
	current, err := a.store.ClusterIdentity(r.Context())
	if err != nil || current.ClusterID != a.identity.ClusterID || current.CAFingerprint != a.identity.CAFingerprint || current.Protocol != a.identity.Protocol {
		writeProductError(w, http.StatusServiceUnavailable, ErrorClusterUnavailable, "cluster enrollment identity is unavailable", nil)
		return false
	}
	return true
}

func (a *clusterEnrollment) invite(w http.ResponseWriter, r *http.Request) {
	var req dto.ClusterInvitationRequest
	if !decodeClusterRequest(w, r, &req) || !a.current(w, r) {
		return
	}
	seconds := int64(900)
	if req.ValidForSeconds != nil {
		seconds = *req.ValidForSeconds
	}
	if seconds < 1 || seconds > 3600 {
		writeProductError(w, 400, ErrorClusterRequestInvalid, "invitation lifetime must be between 1 and 3600 seconds", nil)
		return
	}
	claims, ok := claimsFromRequest(a.store, a.signer, r)
	if !ok {
		writeProductError(w, 401, ErrorAuthenticationRequired, "authentication required", nil)
		return
	}
	invitation, token, err := a.store.IssueClusterInvitationWithOrigin(r.Context(), a.identity.ClusterID, claims.UserID, time.Duration(seconds)*time.Second, req.AuthorizedEnrollmentOrigin)
	if err != nil {
		clusterOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, dto.ClusterInvitationResponse{ClusterID: a.identity.ClusterID, CAFingerprint: a.identity.CAFingerprint, Protocol: a.identity.Protocol, EnrollmentURL: a.certificateURL, AuthorizedEnrollmentOrigin: invitation.AuthorizedEnrollmentOrigin, Token: token, ExpiresAt: invitation.ExpiresAt})
}

func (a *clusterEnrollment) enroll(w http.ResponseWriter, r *http.Request) {
	var req dto.ClusterCertificateRequest
	if !decodeClusterRequest(w, r, &req) || !a.requestIdentity(w, r, req.ClusterID, req.Protocol) {
		return
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		writeProductError(w, 401, ErrorClusterInvitationRequired, "cluster invitation bearer is required", nil)
		return
	}
	key, err := a.authority.CSRKeyFingerprint([]byte(req.CSRPEM))
	if err != nil {
		invalidClusterCertificate(w)
		return
	}
	address, err := store.NormalizeClusterMemberAddress(req.Address)
	if err != nil {
		invalidClusterCertificate(w)
		return
	}
	admission, err := a.store.ClaimClusterInvitationWithOrigin(r.Context(), req.ClusterID, strings.TrimPrefix(values[0], "Bearer "), key, address, req.EnrollmentOrigin)
	if err != nil {
		clusterOperationError(w, err)
		return
	}
	// Sign only after resolving the administrator-approved host from committed
	// admission and invitation state. If signing fails after an unknown response,
	// the same CSR can recover the public certificate without a new bearer.
	_, approved, err := a.store.ClusterCertificateAuthorization(r.Context(), req.ClusterID, key)
	if err != nil {
		clusterOperationError(w, err)
		return
	}
	certificate, err := a.authority.IssueMemberWithAdditionalHost([]byte(req.CSRPEM), key, admission.Address, enrollmentOriginHost(approved), time.Now().UTC(), 30*24*time.Hour)
	if err != nil {
		invalidClusterCertificate(w)
		return
	}
	a.writeCertificate(w, http.StatusCreated, admission, certificate)
}

func (a *clusterEnrollment) recoverCertificate(w http.ResponseWriter, r *http.Request) {
	var req dto.ClusterCertificateRecoveryRequest
	if !decodeClusterRequest(w, r, &req) || !a.requestIdentity(w, r, req.ClusterID, req.Protocol) {
		return
	}
	key, err := a.authority.CSRKeyFingerprint([]byte(req.CSRPEM))
	if err != nil {
		invalidClusterCertificate(w)
		return
	}
	// This is intentionally a public-certificate lookup, not member
	// authentication. No secret material may be returned by this operation.
	admission, approved, err := a.store.ClusterCertificateAuthorization(r.Context(), req.ClusterID, key)
	if err != nil {
		clusterOperationError(w, err)
		return
	}
	certificate, err := a.authority.IssueMemberWithAdditionalHost([]byte(req.CSRPEM), key, admission.Address, enrollmentOriginHost(approved), time.Now().UTC(), 30*24*time.Hour)
	if err != nil {
		invalidClusterCertificate(w)
		return
	}
	a.writeCertificate(w, http.StatusOK, admission, certificate)
}

func enrollmentOriginHost(origin string) string {
	if origin == "" {
		return ""
	}
	parsed, _ := url.Parse(origin) // Store returned a canonical persisted origin.
	return parsed.Hostname()
}

func (a *clusterEnrollment) requestIdentity(w http.ResponseWriter, r *http.Request, clusterID string, protocol uint) bool {
	if !a.current(w, r) {
		return false
	}
	if clusterID != a.identity.ClusterID || protocol != a.identity.Protocol {
		writeProductError(w, http.StatusConflict, ErrorClusterIdentityMismatch, "cluster identity or protocol does not match", nil)
		return false
	}
	return true
}

func (a *clusterEnrollment) writeCertificate(w http.ResponseWriter, status int, admission store.ClusterAdmission, certificate []byte) {
	writeJSON(w, status, dto.ClusterCertificateResponse{ClusterID: a.identity.ClusterID, Protocol: a.identity.Protocol, Address: admission.Address, KeyFingerprint: admission.KeyFingerprint, CertificatePEM: string(certificate), CACertificatePEM: a.caPEM, StorageTier: a.storageTier})
}

func decodeClusterRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 96<<10)
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		invalidClusterCertificate(w)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		invalidClusterCertificate(w)
		return false
	}
	if len(raw) == 0 || raw[0] != '{' {
		invalidClusterCertificate(w)
		return false
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		invalidClusterCertificate(w)
		return false
	}
	return true
}

func invalidClusterCertificate(w http.ResponseWriter) {
	writeProductError(w, http.StatusBadRequest, ErrorClusterRequestInvalid, "invalid cluster enrollment request", nil)
}

func clusterOperationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrClusterAdministratorRequired):
		writeProductError(w, http.StatusForbidden, ErrorAdministratorRequired, "administrator role required", nil)
	case errors.Is(err, store.ErrClusterInvitationRejected), errors.Is(err, store.ErrClusterAdmissionRejected):
		writeProductError(w, http.StatusForbidden, ErrorClusterAdmissionRejected, "cluster invitation or member admission rejected", nil)
	case errors.Is(err, store.ErrClusterEnrollmentOriginInvalid):
		writeProductError(w, http.StatusBadRequest, ErrorClusterRequestInvalid, "invalid cluster enrollment origin", nil)
	default:
		writeProductError(w, http.StatusServiceUnavailable, ErrorClusterUnavailable, "cluster operation failed; query the existing admission before retrying", nil)
	}
}
