package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An enrolled agent lists its cluster's nodes (sinfo) every report interval,
// and the server compares the GPU ones with the nodes that enrolled: the entry
// shows how many GPU nodes Slurm has and which of them no agent serves, and
// why. The other nodes are kept by name, so a node that stops being listed is
// one that left the cluster. The same call hands the agent the entry's current
// settings, which is how an edit reaches a running agent.
//
// Every agent of an entry reports, so the call must be cheap when nothing
// changed, and a list whose states change all the time must not rewrite the
// table with every report. An agent sends two digests, one of the membership
// and one of the GPU nodes' states; the server, which remembers the digests
// it last stored per entry, answers without reading the database when both
// match. When one differs it asks for the list, at most once a minute per
// entry, and the store replaces the rows only when the membership changed,
// else updates the rows whose state did.

// Bounds of a report.
const (
	slurmReportBodyMax = 4 << 20
	slurmReportNodeMax = 20000
)

// slurmStateWriteInterval is the least time between two writes of one entry's
// list: states change all the time and every agent of the entry reports them,
// so a change waits for the next report that finds the interval over.
const slurmStateWriteInterval = time.Minute

// slurmReportMemory is what the server remembers of an entry's list: the
// digests of the list it last stored and when it last wrote.
type slurmReportMemory struct {
	members, states string
	wroteAt         time.Time
}

// slurmReportDigests is the memory of each entry.
type slurmReportDigests struct {
	mu  sync.Mutex
	by  map[uint]slurmReportMemory
	now func() time.Time // nil is time.Now
}

func (d *slurmReportDigests) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now().UTC()
}

func (d *slurmReportDigests) get(cluster uint) slurmReportMemory {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.by[cluster]
}

func (d *slurmReportDigests) forget(cluster uint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.by, cluster)
}

func (d *slurmReportDigests) set(cluster uint, m slurmReportMemory) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.by == nil {
		d.by = map[uint]slurmReportMemory{}
	}
	d.by[cluster] = m
}

// slurmNodeByBearer is the enrolled node whose node token the request carries.
func (a *apiHandlers) slurmNodeByBearer(r *http.Request) (*store.Node, bool, error) {
	token := auth.BearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return nil, false, nil
	}
	node, err := a.d.Store.NodeByTokenHash(store.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return node, node.SlurmClusterID != 0, nil
}

// slurmReportGPUNodes takes an agent's list of its cluster's GPU nodes.
func (a *apiHandlers) slurmReportGPUNodes(w http.ResponseWriter, r *http.Request) {
	node, ok, err := a.slurmNodeByBearer(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeProductError(w, http.StatusUnauthorized, ErrorAuthenticationRequired, "a Slurm node's token is required", nil)
		return
	}
	var req dto.SlurmGPUNodeReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, slurmReportBodyMax)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.Digest, req.StateDigest = strings.TrimSpace(req.Digest), strings.TrimSpace(req.StateDigest)
	if req.Digest == "" || len(req.Digest) > 128 || len(req.StateDigest) > 128 || len(req.Nodes) > slurmReportNodeMax {
		writeFieldInvalid(w, "digest", "a report needs digests of at most 128 bytes and at most 20000 nodes")
		return
	}
	cluster, err := a.d.Store.SlurmClusterByID(node.SlurmClusterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.SlurmGPUNodeReportResponse{Settings: slurmAgentSettings(*cluster)}
	mem := a.slurmReports.get(cluster.ID)
	now := a.slurmReports.clock()
	switch {
	case mem.members == req.Digest && mem.states == req.StateDigest:
		// Nothing to store: this is the list the entry has.
	case now.Sub(mem.wroteAt) < slurmStateWriteInterval:
		// The entry's list was written a moment ago: a change waits for a
		// later report, which brings the same digests again.
	case req.Nodes == nil:
		out.NeedNodes = true
	default:
		rows, bad := slurmReportRows(req.Nodes)
		if bad != "" {
			writeFieldInvalid(w, "nodes", bad)
			return
		}
		change, err := a.d.Store.ReplaceSlurmGPUNodes(cluster.ID, rows)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		next := slurmReportMemory{members: req.Digest, states: req.StateDigest, wroteAt: mem.wroteAt}
		if change.Changed() {
			next.wroteAt = now
		}
		a.slurmReports.set(cluster.ID, next)
	}
	writeJSON(w, http.StatusOK, out)
}

// slurmReportRows turns a report's entries into rows, truncating what Slurm
// prints at the columns' sizes; a report that lists a node twice or none
// names is refused, with why.
func slurmReportRows(entries []dto.SlurmGPUNodeEntry) ([]store.SlurmGPUNode, string) {
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
