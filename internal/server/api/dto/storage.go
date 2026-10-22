package dto

import "time"

// Storage is the API view of a storage.
type Storage struct {
	UpdatedAt      *time.Time      `json:"updated_at,omitempty"`
	ID             uint            `json:"id"`
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	Enabled        bool            `json:"enabled"`
	IsDefault      bool            `json:"is_default"`
	AllNodes       bool            `json:"all_nodes"`
	Path           string          `json:"path"`
	Export         string          `json:"export,omitempty"`
	MountOptions   string          `json:"mount_options,omitempty"`
	CachePath      string          `json:"cache_path,omitempty"`
	CacheMaxBytes  int64           `json:"cache_max_bytes,omitempty"`
	TotalBytes     int64           `json:"total_bytes,omitempty"`
	AvailableBytes int64           `json:"available_bytes,omitempty"`
	Targets        []StorageTarget `json:"targets"`
	Nodes          StorageNodes    `json:"nodes"`
	// Writable is whether any node can write to it (local storages: always).
	Writable      bool   `json:"writable"`
	ModelCount    int    `json:"model_count"`
	DeploymentIDs []uint `json:"deployment_ids"`
}

// StorageTarget applies a storage to a node or to nodes carrying a label.
type StorageTarget struct {
	OverrideOnly  bool   `json:"override_only,omitempty"`
	NodeUUID      string `json:"node_uuid,omitempty"`
	NodeName      string `json:"node_name,omitempty"`
	LabelKey      string `json:"label_key,omitempty"`
	LabelValue    string `json:"label_value,omitempty"`
	Path          string `json:"path,omitempty"`
	CachePath     string `json:"cache_path,omitempty"`
	CacheMaxBytes *int64 `json:"cache_max_bytes,omitempty"`
	Existing      bool   `json:"existing,omitempty"`
}

// StorageNodes summarizes a storage across the nodes it applies to. Problems
// lists only the nodes that are not ready; All is filled on request.
type StorageNodes struct {
	Total    int                 `json:"total"`
	Ready    int                 `json:"ready"`
	Problems []StorageNodeStatus `json:"problems"`
	All      []StorageNodeStatus `json:"all,omitempty"`
}

// StorageNodeStatus is one node's state for a storage.
type StorageNodeStatus struct {
	Cache     *StorageNodeCache `json:"cache,omitempty"`
	NodeUUID  string            `json:"node_uuid"`
	NodeName  string            `json:"node_name"`
	State     string            `json:"state"`
	Reason    string            `json:"reason,omitempty"`
	Message   string            `json:"message,omitempty"`
	Path      string            `json:"path,omitempty"`
	CachePath string            `json:"cache_path,omitempty"`
	Writable  bool              `json:"writable"`
	UpdatedAt *time.Time        `json:"updated_at,omitempty"`
	// Capacity of the storage as this node last measured it (for a local
	// storage, the node's own disk), and of its cache for nfs_cache.
	TotalBytes          int64 `json:"total_bytes,omitempty"`
	AvailableBytes      int64 `json:"available_bytes,omitempty"`
	CacheTotalBytes     int64 `json:"cache_total_bytes,omitempty"`
	CacheAvailableBytes int64 `json:"cache_available_bytes,omitempty"`
	// NeedsOwnDirectory says the node cannot use the storage's directory and
	// needs a per-node one (store.NodeNeedsOwnDirectory).
	NeedsOwnDirectory bool `json:"needs_own_directory,omitempty"`
}

// StorageRequest creates or changes a storage. On a change, absent fields are
// left alone.
type StorageRequest struct {
	ExpectedUpdatedAt *time.Time       `json:"expected_updated_at,omitempty"`
	Name              *string          `json:"name,omitempty"`
	Type              *string          `json:"type,omitempty"`
	Enabled           *bool            `json:"enabled,omitempty"`
	IsDefault         *bool            `json:"is_default,omitempty"`
	AllNodes          *bool            `json:"all_nodes,omitempty"`
	Path              *string          `json:"path,omitempty"`
	Export            *string          `json:"export,omitempty"`
	MountOptions      *string          `json:"mount_options,omitempty"`
	CachePath         *string          `json:"cache_path,omitempty"`
	CacheMaxBytes     *int64           `json:"cache_max_bytes,omitempty"`
	Targets           *[]StorageTarget `json:"targets,omitempty"`
	// ProbeNode is the node a create probes on; empty picks one.
	ProbeNode string `json:"probe_node,omitempty"`
}

// StorageProbeRequest tests a storage definition on one node.
type StorageProbeRequest struct {
	NodeUUID string `json:"node_uuid,omitempty"`
	// Name is the storage's name, for the default mount point when Path is
	// empty (store.DefaultMountPoint).
	Name         string `json:"name,omitempty"`
	Type         string `json:"type"`
	Path         string `json:"path"`
	Export       string `json:"export,omitempty"`
	MountOptions string `json:"mount_options,omitempty"`
	Existing     bool   `json:"existing,omitempty"`
}

// StorageProbeResult is what the probe found and on which node.
type StorageProbeResult struct {
	NodeUUID       string `json:"node_uuid"`
	NodeName       string `json:"node_name"`
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

// StorageScanResult is one page of a shared storage's model directories,
// annotated with what this control plane already knows about each.
type StorageScanResult struct {
	NodeUUID   string                 `json:"node_uuid"`
	Candidates []StorageScanCandidate `json:"candidates"`
	Skipped    []StorageScanSkipped   `json:"skipped"`
	NextCursor string                 `json:"next_cursor,omitempty"`
	Complete   bool                   `json:"complete"`
}

// StorageScanCandidate is one model directory a scan found.
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
	// Imported: this storage already records the directory.
	Imported bool `json:"imported"`
	// BindModelID is the existing model a new import would attach to.
	BindModelID   uint   `json:"bind_model_id,omitempty"`
	BindModelName string `json:"bind_model_name,omitempty"`
}

// StorageScanSkipped is a directory the scan did not offer, with why.
type StorageScanSkipped struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
}

// StorageImportRequest imports scanned directories.
type StorageImportRequest struct {
	Items []StorageImportItem `json:"items"`
}

// StorageImportItem is one directory to import. ModelID binds it to an
// existing model; otherwise a model is found by identity or created.
type StorageImportItem struct {
	RelPath   string `json:"rel_path"`
	ModelID   uint   `json:"model_id,omitempty"`
	Source    string `json:"source,omitempty"`
	SourceID  string `json:"source_id,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Subfolder string `json:"subfolder,omitempty"`
	Name      string `json:"name,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// StorageImportResult reports each imported directory.
type StorageImportResult struct {
	Imported []StorageImported `json:"imported"`
	Failed   []StorageImported `json:"failed"`
}

// StorageImported is one import outcome.
type StorageImported struct {
	RelPath string `json:"rel_path"`
	ModelID uint   `json:"model_id,omitempty"`
	Created bool   `json:"created,omitempty"`
	Error   string `json:"error,omitempty"`
}

// StorageControlPlane is another gpuworks installation using a shared storage.
type StorageControlPlane struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Version         string `json:"version,omitempty"`
	LastSeenSeconds int64  `json:"last_seen_seconds"`
	Fresh           bool   `json:"fresh"`
}

// StorageSettings are the platform-wide storage settings.
type StorageSettings struct {
	ControlPlaneID   string          `json:"control_plane_id"`
	ControlPlaneName string          `json:"control_plane_name"`
	ReserveFreeBytes int64           `json:"reserve_free_bytes"`
	Defaults         StorageDefaults `json:"defaults"`
}

// StorageDefaults are the directories a storage gets when its definition
// leaves them out (store.StorageDefaults): the form shows them instead of
// knowing them. A mount point is <mount_root>/<name>, a node cache
// <cache_root>/<name>.
type StorageDefaults struct {
	LocalPath string `json:"local_path"`
	MountRoot string `json:"mount_root"`
	CacheRoot string `json:"cache_root"`
}

// StorageModelFile is one model file in a storage, as the storage's detail
// page lists them: one row for a shared storage's copy, one row per node for
// a local storage or for an nfs_cache storage's node caches (Cache set).
type StorageModelFile struct {
	ID          uint       `json:"id"`
	ModelID     uint       `json:"model_id"`
	ModelName   string     `json:"model_name"`
	Source      string     `json:"source"`
	SourceID    string     `json:"source_id"`
	NodeUUID    string     `json:"node_uuid,omitempty"`
	NodeName    string     `json:"node_name,omitempty"`
	Cache       bool       `json:"cache,omitempty"`
	Path        string     `json:"path"`
	State       string     `json:"state"`
	Message     string     `json:"message,omitempty"`
	Acquisition string     `json:"acquisition,omitempty"`
	SizeBytes   int64      `json:"size_bytes"`
	Downloaded  int64      `json:"downloaded"`
	Progress    float64    `json:"progress"`
	WriterNode  string     `json:"writer_node,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	// DeploymentIDs are the deployments with an instance reading this model file.
	DeploymentIDs []uint `json:"deployment_ids"`
}
