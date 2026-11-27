package dto

import (
	"time"
)

// Where a model's files are: per node, per shared storage, and the node disks
// that hold them.

// StorageNodeUsage combines control-plane managed bytes with the node's
// statfs snapshot. The two sources are kept explicit for the storage UI.
// CapacityKnown is false until a statfs snapshot reports a positive total.
type StorageNodeUsage struct {
	NodeUUID       string             `json:"node_uuid"`
	Hostname       string             `json:"hostname"`
	CapacityKnown  bool               `json:"capacity_known"`
	ObservedAt     *time.Time         `json:"observed_at"` // Server observation of the path usage report; nil for legacy or absent reports.
	TotalBytes     int64              `json:"total_bytes"`
	AvailableBytes int64              `json:"available_bytes"`
	UsedBytes      int64              `json:"used_bytes"`
	ModelBytes     int64              `json:"model_bytes"`
	ImageBytes     int64              `json:"image_bytes"`
	OtherBytes     int64              `json:"other_bytes"`
	PathUsage      []ManagedPathUsage `json:"path_usage,omitempty"`
}

type ManagedPathUsage struct {
	Kind           string `json:"kind"`
	StorageID      uint   `json:"storage_id,omitempty"`
	Path           string `json:"path"`
	TotalBytes     int64  `json:"total_bytes"`
	AvailableBytes int64  `json:"available_bytes"`
}

// ModelNodePresence shows a model's state on one node.
type ModelNodePresence struct {
	ModelID     uint              `json:"model_id,omitempty"`
	ModelName   string            `json:"model_name,omitempty"`
	Preparation *ModelPreparation `json:"preparation,omitempty"`
	NodeUUID    string            `json:"node_uuid"`
	// NodeName and SourceNodeName name the nodes for display; the uuids remain
	// the identifiers.
	NodeName          string  `json:"node_name,omitempty"`
	SourceNodeName    string  `json:"source_node_name,omitempty"`
	State             string  `json:"state"`
	Path              string  `json:"path"`
	Revision          string  `json:"revision,omitempty"`
	Progress          float64 `json:"progress"` // 0..1
	SizeBytes         int64   `json:"size_bytes"`
	Downloaded        int64   `json:"downloaded"`
	TransferredBytes  int64   `json:"transferred_bytes"`
	RateBps           int64   `json:"rate_bps"`
	RateLimitKiB      int     `json:"rate_limit_kib_s,omitempty"`
	LastRateLimitKiB  int     `json:"last_rate_limit_kib_s,omitempty"`
	Acquisition       string  `json:"acquisition"`
	TransferID        string  `json:"transfer_id,omitempty"`
	TransferErrorCode string  `json:"transfer_error_code,omitempty"`
	SourceNodeUUID    string  `json:"source_node_uuid,omitempty"`
	Message           string  `json:"message,omitempty"`
	// Retry is set while the download waits to ask its address again.
	Retry *ModelFetchRetry `json:"retry,omitempty"`
	// StorageID and StorageName name the storage the files are in; Cache marks
	// an nfs_cache storage's copy on this node.
	StorageID   uint       `json:"storage_id,omitempty"`
	StorageName string     `json:"storage_name,omitempty"`
	StorageType string     `json:"storage_type,omitempty"`
	Cache       bool       `json:"cache,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	// Deletable says these files may be deleted from the node
	// (store.NodeFilesDeletable): not in a read-only local storage. Absent
	// means they may not, or the view (a deployment's progress) does not say.
	Deletable bool `json:"deletable,omitempty"`
}

// ModelStoragePresence is a model's single copy in a shared storage.
type ModelStoragePresence struct {
	StorageID   uint    `json:"storage_id"`
	StorageName string  `json:"storage_name"`
	StorageType string  `json:"storage_type"`
	Path        string  `json:"path"`
	State       string  `json:"state"`
	Acquisition string  `json:"acquisition"`
	Message     string  `json:"message,omitempty"`
	Revision    string  `json:"revision,omitempty"`
	SizeBytes   int64   `json:"size_bytes"`
	Downloaded  int64   `json:"downloaded"`
	Progress    float64 `json:"progress"`
	RateBps     int64   `json:"rate_bps"`
	// ReadyNodes is how many nodes can read it now; CachedNodes is how many
	// nodes an nfs_cache storage has copied it to.
	ReadyNodes  int    `json:"ready_nodes"`
	CachedNodes int    `json:"cached_nodes"`
	WriterNode  string `json:"writer_node,omitempty"`
	// Writable is whether this gpuworks may change the storage now (enabled,
	// and a node can write to it): deleting the files needs it.
	Writable bool `json:"writable"`

	TransferErrorCode string `json:"transfer_error_code,omitempty"`
	// Retry is set while the download waits to ask its address again.
	Retry *ModelFetchRetry `json:"retry,omitempty"`
}

// ModelFetchRetry is a download's next attempt at the address it is at
// (proto.ModelFetchRetry): Attempt of Attempts, WaitSeconds after it was
// reported; Throttled when the address asked for the wait, otherwise Cause
// is the failure's own words, evidence the console shows verbatim.
type ModelFetchRetry struct {
	Attempt     int    `json:"attempt"`
	Attempts    int    `json:"attempts"`
	WaitSeconds int64  `json:"wait_seconds"`
	Throttled   bool   `json:"throttled,omitempty"`
	Cause       string `json:"cause,omitempty"`
}

// ModelPreparation is the API projection of a transient preparation view.
type ModelPreparation struct {
	Phase       string `json:"phase"`
	Reason      string `json:"reason,omitempty"`
	TotalKnown  bool   `json:"total_known"`
	IdleSeconds int64  `json:"idle_seconds"`
	ETASeconds  int64  `json:"eta_seconds,omitempty"`
	// Retryable: a failed cache copy POST .../cache/retry may restart.
	Retryable bool `json:"retryable,omitempty"`
}
