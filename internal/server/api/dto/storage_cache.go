package dto

// StorageNodeCache combines current file facts with the last operation result.
// Zero limit means unlimited; all byte fields are explicitly present on wire.
type StorageNodeCache struct {
	UsedBytes        int64  `json:"used_bytes"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
	ProtectedBytes   int64  `json:"protected_bytes"`
	LimitBytes       int64  `json:"limit_bytes"`
	State            string `json:"state"`
	Reason           string `json:"reason,omitempty"`
	Message          string `json:"message,omitempty"`
}

type StorageCacheReconcileRequest struct {
	NodeUUIDs []string `json:"node_uuids"`
	Mode      string   `json:"mode"`
}
