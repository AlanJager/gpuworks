package store

import "time"

// Network is a user-declared node-to-node network. Its selector is only the
// default: a node attachment may override the interface, IP and HCAs. What the
// fields mean is decided in internal/server/netplan; the store only keeps them.
type Network struct {
	ID          uint   `gorm:"primaryKey"`
	Name        string `gorm:"size:64;uniqueIndex"`
	Description string `gorm:"size:256"`
	Roles       string `gorm:"type:text"`           // JSON []string of netplan roles
	Type        string `gorm:"size:16"`             // a netplan type
	CIDR        string `gorm:"column:cidr;size:64"` // selector: subnet the interface (RDMA: bootstrap) address must be in
	Interface   string `gorm:"size:128"`            // selector: interface name glob; Ethernet networks only
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NetworkAttachment binds a network to one node (NodeID set) or to every node
// carrying a label (LabelKey set). Node attachments outrank label ones; within
// one rank the earliest attachment wins, which is why ID order matters.
type NetworkAttachment struct {
	ID         uint   `gorm:"primaryKey"`
	NetworkID  uint   `gorm:"index"`
	NodeID     uint   `gorm:"index"`
	LabelKey   string `gorm:"size:128"`
	LabelValue string `gorm:"size:256"`
	// Overrides, node attachments only. Interface and IP are the bootstrap
	// address on RDMA networks and the data address otherwise; HCAs pin an
	// RDMA network's rails (empty: inferred from the node's inventory).
	Interface string `gorm:"size:128"`
	IP        string `gorm:"column:ip;size:64"`
	HCAs      string `gorm:"column:hcas;type:text"` // JSON []string of exact RDMA device names
	CreatedAt time.Time
}

// Network test run states.
const (
	NetworkTestRunning     = "running"
	NetworkTestDone        = "done"
	NetworkTestInterrupted = "interrupted"
)

// NetworkTestRun is one internal connectivity test of a network. Results are
// measurements, so they are stored rather than derived.
type NetworkTestRun struct {
	ID          uint   `gorm:"primaryKey"`
	NetworkID   uint   `gorm:"index"`
	Status      string `gorm:"size:16"`
	Kind        string `gorm:"size:16"`  // tcp | rdma_tool; empty means tcp
	Pattern     string `gorm:"size:16"`  // mesh | ring; empty means mesh
	Scope       string `gorm:"size:16"`  // all | nodes | label
	LabelKey    string `gorm:"size:128"` // the label a label-scoped run selected nodes by
	LabelValue  string `gorm:"size:256"`
	Bandwidth   bool
	Seconds     int    // per-pair bandwidth duration
	Nodes       string `gorm:"type:text"` // JSON []string of node UUIDs under test, fixed when the run starts
	NodeCount   int
	PairCount   int
	PassedCount int
	Pairs       string `gorm:"type:text"` // JSON []nettest.Pair, the results
	StartedAt   time.Time
	FinishedAt  *time.Time
}
