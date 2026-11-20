package slurm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// nodeStateTimeout bounds the one scontrol call a status report makes, so a
// controller that does not answer cannot hold the report.
const nodeStateTimeout = 10 * time.Second

// nodeState is what Slurm says about this node's ability to run jobs.
type nodeState struct {
	// code is a proto.SlurmNode* code; empty when Slurm can run jobs here.
	code string
	// reason is Slurm's own Reason text, for display only.
	reason string
}

// reasonStamp is the " [user@time]" Slurm appends to a node's Reason.
var reasonStamp = regexp.MustCompile(`\s\[[^\]\s]*@[^\]]*\]`)

// gresMissingReason is what Slurm writes when slurmd found fewer GPUs than
// the controller's configuration lists (no gres.conf on the node, or no
// NVML plugin to detect them). Slurm's own words are matched here and turned
// into a code at once; nothing past this package reads them.
const gresMissingReason = "count reported lower than configured"

// parseNodeState reads the answer of `scontrol -o show node <node>`: the
// code for the state Slurm will not start jobs in, and Slurm's reason text.
// Precedence: a failed registration (a missing gres.conf first), then down,
// then drained.
func parseNodeState(line string) nodeState {
	line = strings.TrimSpace(line)
	f := fields(line)
	flags := strings.Split(f["State"], "+")
	has := func(name string) bool {
		for _, fl := range flags {
			if strings.EqualFold(strings.TrimRight(fl, "*~#!%$@^-"), name) {
				return true
			}
		}
		return false
	}
	st := nodeState{reason: nodeReason(line)}
	switch {
	case has("INVALID_REG"):
		if strings.HasPrefix(st.reason, "gres/gpu") && strings.Contains(st.reason, gresMissingReason) {
			st.code = proto.SlurmNodeGRESMissing
		} else {
			st.code = proto.SlurmNodeInvalidRegistration
		}
	case has("DOWN") || has("FAIL") || has("NOT_RESPONDING"):
		st.code = proto.SlurmNodeDown
	case has("DRAIN") || has("DRAINING") || has("DRAINED"):
		st.code = proto.SlurmNodeDrained
	}
	if st.code == "" {
		st.reason = "" // the reason of a node that runs jobs is old news
	}
	return st
}

// nodeReason is the Reason of a node record. Unlike the other fields its
// value holds spaces, so it is the text after " Reason=" up to Slurm's
// " [user@time]" stamp, or the end of the line when it has none.
func nodeReason(line string) string {
	_, rest, ok := strings.Cut(line, " Reason=")
	if !ok {
		return ""
	}
	if loc := reasonStamp.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	reason := strings.TrimSpace(rest)
	if reason == "(null)" {
		return ""
	}
	return reason
}

// nodeStateMemory is the last answer Slurm gave about the node, restated
// when a round cannot ask.
type nodeStateMemory struct {
	mu   sync.Mutex
	last nodeState
}

func (m *nodeStateMemory) get() nodeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func (m *nodeStateMemory) set(s nodeState) {
	m.mu.Lock()
	m.last = s
	m.mu.Unlock()
}

// NodeState is this node's Slurm name, the code for why Slurm will not start
// jobs on it (a proto.SlurmNode* code, empty when it will) and Slurm's own
// reason text. When scontrol cannot be asked the last answer is restated:
// a controller that does not answer is no news about the node.
func (l *Leaser) NodeState(ctx context.Context) (node, code, reason string) {
	st, err := l.readNodeState(ctx)
	if err != nil {
		l.log.Debug("Slurm node state unavailable", "node", l.node, "err", err)
		st = l.state.get()
	} else {
		l.state.set(st)
	}
	return l.node, st.code, st.reason
}

func (l *Leaser) readNodeState(ctx context.Context) (nodeState, error) {
	ctx, cancel := context.WithTimeout(ctx, nodeStateTimeout)
	defer cancel()
	out, err := l.run(ctx, "scontrol", "-o", "show", "node", l.node)
	if err != nil {
		return nodeState{}, err
	}
	line := strings.TrimSpace(string(out))
	if fields(line)["NodeName"] != l.node {
		return nodeState{}, fmt.Errorf("scontrol does not know node %q", l.node)
	}
	return parseNodeState(line), nil
}
