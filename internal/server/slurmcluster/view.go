package slurmcluster

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// StalledAfter is how long an entry may wait for its first node before the
// list calls it stalled: the administrator has to run the setup command
// first, and the health check needs a node to register or a reconfigure to
// fire.
const StalledAfter = 10 * time.Minute

// MissingNodesLimit bounds the missing nodes a view lists; the count is of
// all of them.
const MissingNodesLimit = 100

// The states an entry is in, the one value the console shows and filters by.
const (
	StateRevoked = "revoked"
	StateStalled = "stalled"
	StatePending = "pending"
	StateActive  = "active"
)

// The reasons a listed node has no agent online.
const (
	MissingNotEnrolled = "not_enrolled"
	MissingOffline     = "offline"
)

// The blockers of deleting an entry with its nodes.
const (
	BlockerOnline    = "online"
	BlockerInstances = "instances"
)

// View is what the server derives of an entry for its list and detail.
type View struct {
	// Nodes enrolled, of them Online; GPUNodes is how many GPU nodes the
	// entry has as far as the server knows, Missing how many of the listed
	// ones have no agent online (MissingNodes names at most
	// MissingNodesLimit of them).
	Nodes, Online, GPUNodes, Missing int
	MissingNodes                     []MissingNode
	// ListAsOf is how old the list can be when no agent is online to report
	// it; nil otherwise.
	ListAsOf *time.Time
	// Stalled and State: see StalledAfter and the State* values.
	Stalled bool
	State   string
	// Waiting is why an entry with no node, not revoked, has none.
	Waiting Waiting
	// DeleteAllowed: the entry can be deleted with its nodes; else
	// DeleteBlocker is a Blocker* value.
	DeleteAllowed bool
	DeleteBlocker string
	// HasSharedStorage: a storage of the entry's nodes is a shared one.
	HasSharedStorage bool
}

// MissingNode is a listed GPU node with no agent online.
type MissingNode struct {
	Name string
	// Reason is MissingNotEnrolled or MissingOffline (NodeUUID is the node's).
	Reason                  string
	NodeUUID                string
	SlurmState, SlurmReason string
}

// ViewStore is the store as views use it: grouped reads, each one query for
// all the entries asked about.
type ViewStore interface {
	SlurmClusterNodeCounts() (map[uint]store.SlurmNodeCounts, error)
	SlurmGPUNodeCounts() (map[uint]int, error)
	SlurmUnlistedNodeCounts(since time.Time) (map[uint]int, error)
	SlurmMissingNodes(clusterIDs ...uint) ([]store.SlurmMissingNode, error)
	SlurmClusterDeleteBlockers(clusterIDs ...uint) (map[uint]store.SlurmDeleteBlockers, error)
	SlurmClustersWithSharedStorage() (map[uint]bool, error)
	SlurmClustersLastSeen(clusterIDs ...uint) (map[uint]time.Time, error)
}

// Stalled says no node has enrolled in the entry c for longer than
// StalledAfter before now, while it was not joining.
func Stalled(c store.SlurmCluster, now time.Time, joining bool) bool {
	return c.SlurmClusterName == "" && c.RevokedAt == nil && now.Sub(c.CreatedAt) > StalledAfter && !joining
}

// State is where the entry c stands.
func State(c store.SlurmCluster, stalled bool) string {
	switch {
	case c.RevokedAt != nil:
		return StateRevoked
	case stalled:
		return StateStalled
	case c.SlurmClusterName == "":
		return StatePending
	}
	return StateActive
}

// Views derives the view of each entry of rows, in a fixed number of grouped
// queries however many entries and nodes there are. computeNodes says how
// many compute nodes the cloud lists for an entry's cluster (nil when it does
// not say); it is asked only of entries with no node.
func (m *Memory) Views(st ViewStore, rows []store.SlurmCluster, computeNodes func(store.SlurmCluster) *int) (map[uint]View, error) {
	out := make(map[uint]View, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]uint, len(rows))
	for i, c := range rows {
		ids[i] = c.ID
	}
	now := m.clock()
	nodes, err := st.SlurmClusterNodeCounts()
	if err != nil {
		return nil, err
	}
	listed, err := st.SlurmGPUNodeCounts()
	if err != nil {
		return nil, err
	}
	unlisted, err := st.SlurmUnlistedNodeCounts(now.Add(-ListLag))
	if err != nil {
		return nil, err
	}
	missing, err := st.SlurmMissingNodes(ids...)
	if err != nil {
		return nil, err
	}
	blockers, err := st.SlurmClusterDeleteBlockers(ids...)
	if err != nil {
		return nil, err
	}
	withShared, err := st.SlurmClustersWithSharedStorage()
	if err != nil {
		return nil, err
	}
	// With no agent online the list is the one from before they went: say
	// how old it can be. Only such entries are read for it.
	var silent []uint
	for _, c := range rows {
		if n := nodes[c.ID]; n.Enrolled > 0 && n.Online == 0 && listed[c.ID] > 0 {
			silent = append(silent, c.ID)
		}
	}
	lastSeen, err := st.SlurmClustersLastSeen(silent...)
	if err != nil {
		return nil, err
	}
	for _, c := range rows {
		// An agent is only installed on a node that has a GPU, so a node that
		// enrolled is a GPU node of the entry even before the list shows it:
		// the list is stored at most once per StateWriteInterval and each
		// agent reports every ReportInterval. Nodes that enrolled within that
		// lag and are not on the list yet count with it; an older node the
		// list leaves out has left the cluster (or has no gpu GRES) and does
		// not.
		v := View{Nodes: nodes[c.ID].Enrolled, Online: nodes[c.ID].Online, GPUNodes: listed[c.ID] + unlisted[c.ID],
			MissingNodes: []MissingNode{}, HasSharedStorage: withShared[c.ID]}
		if seen, ok := lastSeen[c.ID]; ok {
			v.ListAsOf = &seen
		}
		if c.RevokedAt == nil && v.Nodes == 0 {
			var cloud *int
			if computeNodes != nil {
				cloud = computeNodes(c)
			}
			v.Waiting = m.Waiting(c, cloud)
		}
		v.Stalled = Stalled(c, now, v.Waiting.Joining)
		v.State = State(c, v.Stalled)
		b := blockers[c.ID]
		v.DeleteAllowed = b == (store.SlurmDeleteBlockers{})
		switch {
		case b.Online > 0:
			v.DeleteBlocker = BlockerOnline
		case b.Instances > 0:
			v.DeleteBlocker = BlockerInstances
		}
		out[c.ID] = v
	}
	for _, n := range missing {
		v := out[n.ClusterID]
		v.Missing++
		if len(v.MissingNodes) < MissingNodesLimit {
			mn := MissingNode{Name: n.Name, Reason: MissingNotEnrolled, SlurmState: n.State, SlurmReason: n.Reason}
			if n.Enrolled {
				mn.Reason, mn.NodeUUID = MissingOffline, n.NodeUUID
			}
			v.MissingNodes = append(v.MissingNodes, mn)
		}
		out[n.ClusterID] = v
	}
	return out, nil
}
