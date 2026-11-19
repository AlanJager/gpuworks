package slurmcluster

import (
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An enrolled agent lists its cluster's nodes (sinfo) every report interval,
// and the server compares the GPU ones with the nodes that enrolled: the entry
// shows how many GPU nodes Slurm has and which of them no agent serves, and
// why. The other nodes are kept by name, so a node that stops being listed is
// one that left the cluster.
//
// Every agent of an entry reports, so a report must be cheap when nothing
// changed, and a list whose states change all the time must not rewrite the
// table with every report. An agent sends two digests, one of the membership
// and one of the GPU nodes' states; the server, which remembers the digests
// it last stored per entry, answers with one indexed count when both match.
// When one differs it asks for the list, at most once per StateWriteInterval
// per entry, and the store replaces the rows only when the membership
// changed, else updates the rows whose state did. One entry's reports are
// taken one at a time, so two agents cannot both find the interval over.

// StateWriteInterval is the least time between two writes of one entry's
// list: states change all the time and every agent of the entry reports them,
// so a change waits for the next report that finds the interval over.
const StateWriteInterval = time.Minute

// ReportInterval is how often an agent reports the cluster's nodes and, with
// the answer, picks up an edit of its entry's settings.
const ReportInterval = proto.SlurmReportIntervalSeconds * time.Second

// ListLag is how long a node that has enrolled may be missing from the list
// the agents report: an agent reports every ReportInterval and the entry's
// list is stored at most once per StateWriteInterval.
const ListLag = ReportInterval + StateWriteInterval

// listMemory is what the server remembers of an entry's list: the digests of
// the list it last stored and when it last wrote. mu takes the entry's
// reports one at a time.
type listMemory struct {
	mu              sync.Mutex
	members, states string
	wroteAt         time.Time
	// rows is how many rows the stored list had when these digests were
	// stored: a node deleted since (by hand or by the sweep) took its row
	// with it, and the next report stores the list whole again.
	rows int
}

// ReportStore is the store as report ingestion uses it.
type ReportStore interface {
	SlurmListRowCount(clusterID uint) (int, error)
	ReplaceSlurmGPUNodes(clusterID uint, nodes []store.SlurmGPUNode) (store.SlurmListChange, error)
}

// BadNodesError is a report whose nodes cannot be stored, and why.
type BadNodesError struct{ Reason string }

func (e *BadNodesError) Error() string { return e.Reason }

// listOf is the entry's list memory, created when it has none.
func (m *Memory) listOf(cluster uint) *listMemory {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reports == nil {
		m.reports = map[uint]*listMemory{}
	}
	l := m.reports[cluster]
	if l == nil {
		l = &listMemory{}
		m.reports[cluster] = l
	}
	return l
}

// Ingest takes an agent's report of the entry cluster's nodes and says
// whether the server wants the list the digests name (the agent sends it
// again with the nodes). It stores a list only when the digests differ from
// the stored one's and the last write was StateWriteInterval ago or more. A
// list that names a node twice or none, or a nameless one, is a
// *BadNodesError.
func (m *Memory) Ingest(st ReportStore, cluster uint, rep proto.SlurmGPUNodeReport) (needNodes bool, err error) {
	l := m.listOf(cluster)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := m.clock()
	same := l.members == rep.Digest && l.states == rep.StateDigest
	if same {
		// The digests are the ones stored; the rows may not be, when a node
		// was deleted since (one indexed count).
		rows, err := st.SlurmListRowCount(cluster)
		if err != nil {
			return false, err
		}
		if rows != l.rows {
			same, l.wroteAt = false, time.Time{}
		}
	}
	switch {
	case same:
		// Nothing to store: this is the list the entry has.
		return false, nil
	case now.Sub(l.wroteAt) < StateWriteInterval:
		// The entry's list was written a moment ago: a change waits for a
		// later report, which brings the same digests again.
		return false, nil
	case rep.Nodes == nil:
		return true, nil
	}
	rows, bad := reportRows(rep.Nodes)
	if bad != "" {
		return false, &BadNodesError{Reason: bad}
	}
	change, err := st.ReplaceSlurmGPUNodes(cluster, rows)
	if err != nil {
		return false, err
	}
	l.members, l.states, l.rows = rep.Digest, rep.StateDigest, len(rows)
	if change.Changed() {
		l.wroteAt = now
	}
	return false, nil
}

// reportRows turns a report's entries into rows, truncating what Slurm
// prints at the columns' sizes; a report that lists a node twice or none
// names is refused, with why.
func reportRows(entries []proto.SlurmGPUNodeEntry) ([]store.SlurmGPUNode, string) {
	rows := make([]store.SlurmGPUNode, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		if name == "" || len(name) > 64 {
			return nil, "every node needs a name of at most 64 bytes"
		}
		if seen[name] {
			return nil, "node " + name + " is listed twice"
		}
		seen[name] = true
		if e.ListedOnly {
			rows = append(rows, store.SlurmGPUNode{Name: name, ListedOnly: true})
			continue
		}
		rows = append(rows, store.SlurmGPUNode{Name: name, State: truncateBytes(strings.ToLower(strings.TrimSpace(e.State)), 32),
			Reason: truncateBytes(strings.TrimSpace(e.Reason), 256), GPUs: max(e.GPUs, 0)})
	}
	return rows, ""
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // do not cut a UTF-8 sequence
		n--
	}
	return s[:n]
}
