package dto

import "time"

type ClusterInvitationRequest struct {
	ValidForSeconds            *int64 `json:"valid_for_seconds,omitempty"`
	AuthorizedEnrollmentOrigin string `json:"authorized_enrollment_origin,omitempty"`
}

type ClusterInvitationResponse struct {
	ClusterID                  string    `json:"cluster_id"`
	CAFingerprint              string    `json:"ca_fingerprint"`
	Protocol                   uint      `json:"protocol"`
	EnrollmentURL              string    `json:"enrollment_url"`
	AuthorizedEnrollmentOrigin string    `json:"authorized_enrollment_origin,omitempty"`
	Token                      string    `json:"token"`
	ExpiresAt                  time.Time `json:"expires_at"`
}

type ClusterCertificateRequest struct {
	ClusterID        string `json:"cluster_id"`
	Protocol         uint   `json:"protocol"`
	Address          string `json:"address"`
	EnrollmentOrigin string `json:"enrollment_origin,omitempty"`
	CSRPEM           string `json:"csr_pem"`
}

type ClusterCertificateRecoveryRequest struct {
	ClusterID string `json:"cluster_id"`
	Protocol  uint   `json:"protocol"`
	CSRPEM    string `json:"csr_pem"`
}

// ClusterCertificateResponse contains only public identity material. It does
// not grant voter/readiness status and must never grow private-key fields.
type ClusterCertificateResponse struct {
	ClusterID        string `json:"cluster_id"`
	Protocol         uint   `json:"protocol"`
	Address          string `json:"address"`
	KeyFingerprint   string `json:"key_fingerprint"`
	CertificatePEM   string `json:"certificate_pem"`
	CACertificatePEM string `json:"ca_certificate_pem"`
	// StorageTier is the timing profile every member of this cluster runs,
	// absent for the standard one. A joining member must start with it.
	StorageTier string `json:"storage_tier,omitempty"`
}
