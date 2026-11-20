package enrollment

// SecretsPath is an internal member RPC, never a management API or download URL.
const SecretsPath = "/_cluster/bootstrap-secrets"

// BootstrapSecrets transfers cluster cryptographic material to an admitted
// member over mutually authenticated TLS. It is not a complete asset replica
// and must never be logged, displayed, or used to mark a member ready.
type BootstrapSecrets struct {
	ClusterID        string `json:"cluster_id"`
	Protocol         uint   `json:"protocol"`
	CACertificatePEM string `json:"ca_certificate_pem"`
	CAKeyPEM         string `json:"ca_key_pem"`
	MasterSecret     []byte `json:"master_secret_base64"`
}
