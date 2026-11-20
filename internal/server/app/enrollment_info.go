package app

// EnrollmentInfo is the public, persisted enrollment listener configuration.
// It is not Raft membership, a readiness flag or a substitute for admission.
type EnrollmentInfo struct {
	ClusterID     string `json:"cluster_id"`
	CAFingerprint string `json:"ca_fingerprint"`
	Protocol      uint   `json:"protocol"`
	ListenAddress string `json:"listen_address"`
	PublicURL     string `json:"public_url"`
	// StorageTier is the cluster's timing profile as storagetier records it:
	// absent for the standard tier. Every member of a cluster carries the
	// same value; it is not part of the cluster's identity.
	StorageTier string `json:"storage_tier,omitempty"`
}
