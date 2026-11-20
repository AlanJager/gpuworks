package app

// ClusterJoinTarget is the public, administrator-supplied destination. It
// contains no invitation token; interrupted joins retain only this identity.
type ClusterJoinTarget struct {
	ClusterID        string `json:"cluster_id"`
	CAFingerprint    string `json:"ca_fingerprint"`
	Protocol         uint   `json:"protocol"`
	EnrollmentURL    string `json:"enrollment_url"`
	EnrollmentOrigin string `json:"enrollment_origin,omitempty"`
	EnrollmentListen string `json:"enrollment_listen,omitempty"`
	Address          string `json:"address"`
}

// ClusterJoinInfo binds the destination to a durable local member key. It does
// not imply admission, Raft membership, material synchronization or readiness.
type ClusterJoinInfo struct {
	ClusterJoinTarget
	KeyFingerprint string `json:"key_fingerprint"`
}

// ClusterCatchUp is a live membership observation. FaultTolerant is true only
// when at least three voters are visible and this member is one of them.
// MaterialsReady is the material-dependent step, not application readiness:
// it is true only when the required digest survives one member fault.
type ClusterCatchUp struct {
	ID             uint64 `json:"id"`
	Address        string `json:"address"`
	Role           string `json:"role"`
	Voters         int    `json:"voters"`
	FaultTolerant  bool   `json:"fault_tolerant"`
	MaterialsReady bool   `json:"materials_ready"`
}
