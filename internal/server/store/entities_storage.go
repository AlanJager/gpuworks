package store

import "time"

// Storage is where model files live. Its type decides where the authoritative
// copy is and how a node gets a readable path to it (see proto.Storage*).
type Storage struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:128;uniqueIndex"`
	Type string `gorm:"size:16;index"` // proto.StorageLocal | StorageNFS | StorageNFSCache | StorageSharedDir
	// Enabled false makes the storage read-only: nothing is downloaded,
	// copied into or deleted from it; model files already there stay usable.
	Enabled bool `gorm:"index"`
	// IsDefault is what every "save to" choice preselects; at most one row.
	IsDefault bool `gorm:"index"`
	// AllNodes applies the storage to every node; otherwise only to the
	// targets listed in storage_nodes.
	AllNodes bool
	// Path is a local storage's directory, a shared storage's mount point or
	// an existing shared directory; a node override may replace it.
	Path         string `gorm:"size:512"`
	Export       string `gorm:"size:512;index"` // nfs / nfs_cache: host:/path, unique across rows
	MountOptions string `gorm:"size:255"`
	// CachePath is an nfs_cache storage's node cache directory
	// (DefaultCachePath unless set).
	CachePath string `gorm:"size:512"`
	// CacheMaxBytes caps an nfs_cache storage's cache on each node; 0 = no cap.
	CacheMaxBytes int64
	// TotalBytes and AvailableBytes are a shared storage's capacity as one
	// node last reported it; a change under 1% is not written.
	TotalBytes     int64
	AvailableBytes int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// StorageNode applies a storage to a node or to every node carrying a label,
// and carries a node's overrides. OverrideOnly rows never grant membership.
type StorageNode struct {
	ID         uint   `gorm:"primaryKey"`
	StorageID  uint   `gorm:"index:idx_storage_node,priority:1"`
	NodeID     uint   `gorm:"index:idx_storage_node,priority:2"` // 0 on a label row
	LabelKey   string `gorm:"size:128"`
	LabelValue string `gorm:"size:256"`
	// OverrideOnly carries configuration without explicitly selecting the node.
	// False preserves the membership semantics of existing targets.
	OverrideOnly bool `gorm:"not null;default:false"`
	// The following apply to node rows only.
	Path          string `gorm:"size:512"`
	CachePath     string `gorm:"size:512"`
	CacheMaxBytes *int64 // nil follows the storage
	// Existing uses a mount already on the node: the agent neither mounts nor
	// unmounts it.
	Existing  bool
	CreatedAt time.Time
}

// StorageNodeState is a node's latest report for one storage. It is written
// only when a reported value changes.
type StorageNodeState struct {
	ID        uint   `gorm:"primaryKey"`
	StorageID uint   `gorm:"index:idx_storage_node_state,unique,priority:1"`
	NodeID    uint   `gorm:"index:idx_storage_node_state,unique,priority:2"`
	State     string `gorm:"size:16;index"` // proto.StorageState*
	Reason    string `gorm:"size:32"`
	Message   string `gorm:"size:512"`
	Path      string `gorm:"size:512"` // the directory or mount point in effect on the node
	CachePath string `gorm:"size:512"`
	Writable  bool
	UpdatedAt time.Time
}
