package cluster

// MaintenanceCheckpoint is a content observation, not a follower persistence
// acknowledgement. Only the original native leader can publish its final proof.
type MaintenanceCheckpoint struct {
	Version       int `json:"version"`
	SchemaVersion int `json:"schema_version"`
	Phase         struct {
		Phase    string
		Revision int64
	} `json:"phase"`
	SchemaDigest string                   `json:"schema_digest"`
	Digest       string                   `json:"digest"`
	Tables       []MaintenanceTableDigest `json:"tables"`
	Proof        QuorumProof              `json:"proof"`
}

type MaintenanceTableDigest struct {
	Name   string `json:"name"`
	Rows   uint64 `json:"rows"`
	SHA256 string `json:"sha256"`
}
