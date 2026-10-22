package fleet

import (
	"sort"
	"sync"
	"time"
)

// NodeState is the part of a node the load case cares about: whether the
// control plane still counts it as live, and when it last heard from it.
type NodeState struct {
	Name       string     `json:"name"`
	UUID       string     `json:"uuid"`
	State      string     `json:"state"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

// NodeStateOffline is the state a node the control plane has given up on
// reports. It is the wire value, so the driver does not need the store package.
const NodeStateOffline = "offline"

// NodeStateOnline is the state of a node with a live session.
const NodeStateOnline = "online"

// HeartbeatStarvation is the heartbeat age that counts as a live node the
// control plane has stopped hearing from. Agents beat every 15 seconds and the
// server sweeps at 90, so four missed beats is well clear of normal jitter and
// still catches the problem before the sweeper turns it into an offline node.
const HeartbeatStarvation = 60 * time.Second

// Latencies collects durations and answers percentile questions about them.
type Latencies struct {
	mu     sync.Mutex
	values []time.Duration
}

// Add records one measurement.
func (l *Latencies) Add(d time.Duration) {
	l.mu.Lock()
	l.values = append(l.values, d)
	l.mu.Unlock()
}

// Count is how many measurements were taken, which says whether a percentile
// means anything.
func (l *Latencies) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.values)
}

// Percentile returns the requested quantile, or zero when nothing was measured.
func (l *Latencies) Percentile(p float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), l.values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(float64(len(sorted)-1)*p)]
}

// NodeWatch samples node liveness during the churn. It separates the two things
// that look alike from the outside: a node the control plane has stopped
// hearing from, and a session that dropped and came straight back.
type NodeWatch struct {
	// Snapshot reads the fleet's current state. Both shapes read it over the
	// same REST endpoint an operator's dashboard polls.
	Snapshot func() ([]NodeState, error)
	// OnStall, when set, is called with a label the first time a node goes
	// offline and whenever an online node's heartbeat crosses the trigger. It is
	// how a run under diagnosis captures what the process was doing.
	OnStall func(label string)
	// StallTrigger is the heartbeat age OnStall reports. Zero disables it.
	StallTrigger time.Duration

	mu       sync.Mutex
	starved  map[string]time.Duration
	flapped  map[string]bool
	flaps    int
	failures int
}

// Run samples once a second until stop is closed.
func (w *NodeWatch) Run(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	w.mu.Lock()
	w.starved, w.flapped = map[string]time.Duration{}, map[string]bool{}
	w.mu.Unlock()
	for {
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
		}
		rows, err := w.Snapshot()
		if err != nil {
			w.mu.Lock()
			w.failures++
			w.mu.Unlock()
			continue
		}
		w.sample(rows)
	}
}

func (w *NodeWatch) sample(rows []NodeState) {
	worst, worstName := time.Duration(0), ""
	var newlyOffline []string
	w.mu.Lock()
	for _, row := range rows {
		switch {
		case row.State == NodeStateOffline:
			if !w.flapped[row.Name] {
				w.flapped[row.Name] = true
				w.flaps++
				newlyOffline = append(newlyOffline, row.Name)
			}
		case row.State == NodeStateOnline && row.LastSeenAt != nil:
			gap := time.Since(*row.LastSeenAt)
			if gap > HeartbeatStarvation && gap > w.starved[row.Name] {
				w.starved[row.Name] = gap
			}
			if gap > worst {
				worst, worstName = gap, row.Name
			}
		}
	}
	w.mu.Unlock()
	if w.OnStall == nil {
		return
	}
	for _, name := range newlyOffline {
		w.OnStall("offline-" + name)
	}
	if w.StallTrigger > 0 && worst >= w.StallTrigger {
		w.OnStall("heartbeat-" + worstName + "-" + worst.Round(time.Millisecond).String())
	}
}

// Starved lists the live nodes whose heartbeat went stale, worst age each.
func (w *NodeWatch) Starved() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.starved))
	for name, gap := range w.starved {
		out = append(out, name+" ("+gap.Round(time.Second).String()+")")
	}
	sort.Strings(out)
	return out
}

// Flaps is how many distinct nodes were seen offline at least once.
func (w *NodeWatch) Flaps() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flaps
}

// SnapshotFailures is how many liveness reads failed outright. A watch that
// could not read anything would otherwise report a suspiciously clean run.
func (w *NodeWatch) SnapshotFailures() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failures
}
