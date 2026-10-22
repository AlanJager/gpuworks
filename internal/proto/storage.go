package proto

import "strings"

// Storage types. A storage is where model files live; its type decides where
// the authoritative copy is and how a node gets a readable path to it.
const (
	StorageLocal     = "local"      // a directory on each node; each node holds its own model files
	StorageNFS       = "nfs"        // an NFS export the agent mounts; one copy shared by every node
	StorageNFSCache  = "nfs_cache"  // an NFS export plus a per-node local cache that launches read from
	StorageSharedDir = "shared_dir" // a shared directory already mounted on the node; behaves as nfs
)

// StorageShared reports whether one model file of this storage type serves
// every node that can reach the storage.
func StorageShared(storageType string) bool {
	return storageType == StorageNFS || storageType == StorageNFSCache || storageType == StorageSharedDir
}

// Storage states as a node reports them.
const (
	StorageStateReady        = "ready"
	StorageStateMounting     = "mounting"
	StorageStateFailed       = "failed"
	StorageStateUnresponsive = "unresponsive"
	StorageStateUnsupported  = "unsupported"
)

// Reasons a node gives for a failed storage that the server acts on (the
// agent's storagemount reports them; a test there keeps them equal).
const (
	StorageReasonPermissionDenied = "permission_denied"
	StorageReasonPathMissing      = "path_missing"
)

// TStatusStorage carries the node's full storage status list. The agent sends
// it after every desired-state sync and whenever a status changes.
const TStatusStorage = "status.storage"

// Storage RPCs.
const (
	RPCStorageProbe  = "storage.probe"
	RPCStorageScan   = "storage.scan"
	RPCStoragePeers  = "storage.peers"
	RPCStorageRemove = "storage.remove"
)

// StorageTask is one storage the server wants usable on this node, already
// merged with the node's overrides.
type StorageTask struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// Path is the local directory, the NFS mount point or the existing shared
	// directory. The server always sends it, already merged with the node's
	// override; the agent has no default of its own, and an empty Path is
	// reported as a failed storage (path_missing).
	Path         string `json:"path"`
	Export       string `json:"export,omitempty"`
	MountOptions string `json:"mount_options,omitempty"`
	Existing     bool   `json:"existing,omitempty"`
	// CachePath is the node's cache directory of an nfs_cache storage. The
	// server always sends it for that type; an empty one is reported as a
	// failed storage (path_missing).
	CachePath string `json:"cache_path,omitempty"`
	// Heartbeat is sent only to the one node the server picked to keep this
	// control plane's heartbeat file on a shared storage fresh.
	Heartbeat *StorageHeartbeat `json:"heartbeat,omitempty"`
}

// StorageHeartbeat names this control plane on a shared storage.
type StorageHeartbeat struct {
	ControlPlaneID   string `json:"control_plane_id"`
	ControlPlaneName string `json:"control_plane_name"`
	// Version is the control plane's build version, written into the
	// heartbeat file as is.
	Version string `json:"version,omitempty"`
	// IntervalSeconds is how often the node refreshes the heartbeat; it must
	// be positive.
	IntervalSeconds int `json:"interval_seconds"`
}

// StorageStatus is one storage's state on the reporting node.
type StorageStatus struct {
	ID                  uint   `json:"id"`
	State               string `json:"state"`
	Reason              string `json:"reason,omitempty"`
	Message             string `json:"message,omitempty"`
	Path                string `json:"path,omitempty"`
	Writable            bool   `json:"writable"`
	TotalBytes          int64  `json:"total_bytes,omitempty"`
	AvailableBytes      int64  `json:"available_bytes,omitempty"`
	CachePath           string `json:"cache_path,omitempty"`
	CacheTotalBytes     int64  `json:"cache_total_bytes,omitempty"`
	CacheAvailableBytes int64  `json:"cache_available_bytes,omitempty"`
}

// StorageStatusReport is the payload of status.storage. It is the node's full
// list, so a storage missing from it is no longer present on the node.
type StorageStatusReport struct {
	Storages []StorageStatus `json:"storages"`
}

// StorageProbeParams tests a storage definition on one node before it is saved.
type StorageProbeParams struct {
	Type         string `json:"type"`
	Path         string `json:"path"`
	Export       string `json:"export,omitempty"`
	MountOptions string `json:"mount_options,omitempty"`
	Existing     bool   `json:"existing,omitempty"`
}

// StorageProbeResult is what a probe found. Nothing stays mounted afterwards.
type StorageProbeResult struct {
	OK             bool   `json:"ok"`
	Reason         string `json:"reason,omitempty"`
	Message        string `json:"message,omitempty"`
	Output         string `json:"output,omitempty"`
	Version        string `json:"version,omitempty"`
	Options        string `json:"options,omitempty"`
	Readable       bool   `json:"readable"`
	Writable       bool   `json:"writable"`
	TotalBytes     int64  `json:"total_bytes,omitempty"`
	AvailableBytes int64  `json:"available_bytes,omitempty"`
}

// StorageScanParams pages through the model directories of a shared storage.
type StorageScanParams struct {
	StorageID uint   `json:"storage_id"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	MaxDepth  int    `json:"max_depth,omitempty"`
	// TimeoutSeconds bounds one page.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// StorageScanCandidate is one directory that passed the model structure check.
type StorageScanCandidate struct {
	RelPath          string   `json:"rel_path"`
	SizeBytes        int64    `json:"size_bytes"`
	Files            int      `json:"files"`
	Source           string   `json:"source"`
	SourceID         string   `json:"source_id"`
	Revision         string   `json:"revision,omitempty"`
	Subfolder        string   `json:"subfolder,omitempty"`
	Format           string   `json:"format,omitempty"`
	Tasks            []string `json:"tasks,omitempty"`
	MetadataIssues   []string `json:"metadata_issues,omitempty"`
	Basis            string   `json:"basis"`
	Incomplete       bool     `json:"incomplete,omitempty"`
	IncompleteReason string   `json:"incomplete_reason,omitempty"`
}

// StorageScanSkipped is a directory the scan looked at and did not offer.
type StorageScanSkipped struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
}

// Reasons a scanned directory is not offered as a model.
const (
	ScanSkipNoWeights          = "no_weights"
	ScanSkipEmpty              = "empty"
	ScanSkipMetadataOnly       = "metadata_only"
	ScanSkipHFHubCache         = "hf_hub_cache_layout"
	ScanSkipTooDeep            = "too_deep"
	ScanSkipUnreadable         = "unreadable"
	ScanSkipComponent          = "model_component"
	ScanSkipMultipleEntries    = "multiple_model_entries"
	ScanSkipInvalidMetadata    = "invalid_model_metadata"
	ScanSkipMetadataLimit      = "metadata_read_limit"
	ScanSkipUnrecognizedLayout = "unrecognized_model_layout"
)

// ScanSkipUndecided reports whether a skip reason says nothing about whether
// the directory holds a model: the scan could not read it or did not go in.
func ScanSkipUndecided(reason string) bool {
	switch reason {
	case ScanSkipNoWeights, ScanSkipEmpty, ScanSkipMetadataOnly:
		return false
	}
	return true
}

// StorageScanResult is one page of a scan. A page covers the relative paths
// after the request's Cursor up to NextCursor (to the end when Complete), in
// ScanOrder.
type StorageScanResult struct {
	Candidates []StorageScanCandidate `json:"candidates"`
	Skipped    []StorageScanSkipped   `json:"skipped"`
	NextCursor string                 `json:"next_cursor,omitempty"`
	Complete   bool                   `json:"complete"`
}

// ScanOrder compares two slash-separated relative paths in the order a scan
// visits them: shallower first, then component by component. Scan cursors are
// positions in this order.
func ScanOrder(a, b string) int {
	ac, bc := strings.Split(a, "/"), strings.Split(b, "/")
	if len(ac) != len(bc) {
		return len(ac) - len(bc)
	}
	for i := range ac {
		if c := strings.Compare(ac[i], bc[i]); c != 0 {
			return c
		}
	}
	return 0
}

// ScanCovers reports whether a page scanned from cursor covers rel.
func ScanCovers(cursor string, page StorageScanResult, rel string) bool {
	if cursor != "" && ScanOrder(rel, cursor) <= 0 {
		return false
	}
	return page.Complete || (page.NextCursor != "" && ScanOrder(rel, page.NextCursor) <= 0)
}

// StoragePeersParams refreshes this control plane's heartbeat on a shared
// storage and lists the other control planes that use it.
type StoragePeersParams struct {
	StorageID        uint   `json:"storage_id"`
	ControlPlaneID   string `json:"control_plane_id"`
	ControlPlaneName string `json:"control_plane_name"`
	Version          string `json:"version,omitempty"`
	// FreshSeconds is how recent a heartbeat counts as fresh; it must be
	// positive unless Remove is set.
	FreshSeconds int `json:"fresh_seconds"`
	// Remove deletes this control plane's heartbeat instead of refreshing it.
	Remove bool `json:"remove,omitempty"`
}

// StoragePeer is another control plane's heartbeat on a shared storage.
type StoragePeer struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Version         string `json:"version,omitempty"`
	LastSeenSeconds int64  `json:"last_seen_seconds"`
	Fresh           bool   `json:"fresh"`
}

// StoragePeersResult lists the other control planes' heartbeats.
type StoragePeersResult struct {
	Peers []StoragePeer `json:"peers"`
}

// StorageRemoveParams deletes one model directory from a shared storage.
type StorageRemoveParams struct {
	StorageID uint   `json:"storage_id"`
	RelPath   string `json:"rel_path"`
}
