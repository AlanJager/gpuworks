package pluginhost

import (
	"bufio"
	"io"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Call classes as the metrics label them (Class).
const (
	classMember = iota
	classAdmin
	classMemberStream
	classAdminStream
	numClasses
)

var classNames = [numClasses]string{"member", "admin", "member_stream", "admin_stream"}

func classIndex(c Class) int {
	i := classMember
	if c.Admin {
		i = classAdmin
	}
	if c.Stream {
		i += classMemberStream
	}
	return i
}

// CallOutcome is how a call through Do ended, as the metrics label it: ok
// (the plugin answered, its own refusals included), or the refusal Do
// returned.
type CallOutcome uint8

const (
	CallOK          CallOutcome = iota
	CallUnavailable             // PLUGIN_UNAVAILABLE
	CallBusy                    // PLUGIN_BUSY
	CallTimeout                 // PLUGIN_TIMEOUT
	CallFailed                  // PLUGIN_ROUTE_FAILED
	numOutcomes
)

var outcomeNames = [numOutcomes]string{"ok", "unavailable", "busy", "timeout", "failed"}

// callSecondsBounds are the upper bounds of gpuworks_plugin_call_seconds;
// a call ends at most at the call timeout (60 s by default).
var callSecondsBounds = [...]float64{0.005, 0.025, 0.1, 0.25, 1, 2.5, 10, 30, 60}

// tableKinds are the pushed table kinds counted per plugin, sorted; a kind
// outside them is not counted.
var tableKinds = extapi.TableKinds()

// pluginMetrics are one package's counters. They outlive its processes and
// its disable and enable; ForgetMetrics drops them when it is uninstalled.
// Every update is an atomic add on a fixed slot, so counting a call
// allocates nothing.
type pluginMetrics struct {
	restarts atomic.Uint64
	calls    [numClasses][numOutcomes]atomic.Uint64
	// buckets counts the calls whose duration fell in each bound (not
	// cumulative); the last slot is above every bound.
	buckets  [numClasses][len(callSecondsBounds) + 1]atomic.Uint64
	sumNanos [numClasses]atomic.Uint64
	pushRows []atomic.Uint64 // by tableKinds
}

func newPluginMetrics() *pluginMetrics {
	return &pluginMetrics{pushRows: make([]atomic.Uint64, len(tableKinds))}
}

// observeCall counts one call of class that ended with outcome after d.
func (m *pluginMetrics) observeCall(class Class, outcome CallOutcome, d time.Duration) {
	c := classIndex(class)
	m.calls[c][outcome].Add(1)
	secs := d.Seconds()
	b := 0
	for b < len(callSecondsBounds) && secs > callSecondsBounds[b] {
		b++
	}
	m.buckets[c][b].Add(1)
	if d > 0 {
		m.sumNanos[c].Add(uint64(d))
	}
}

// addPushRows counts n entries of a kind applied from a push.
func (m *pluginMetrics) addPushRows(kind string, n int) {
	for i, k := range tableKinds {
		if k == kind {
			m.pushRows[i].Add(uint64(max(n, 0)))
			return
		}
	}
}

func (h *Host) metricsLocked(id string) *pluginMetrics {
	m := h.metrics[id]
	if m == nil {
		m = newPluginMetrics()
		h.metrics[id] = m
	}
	return m
}

// CountPushRows counts n entries of kind that package id's push applied
// (gpuworks_plugin_push_rows_total). Only a package the host started has
// counters: a push committed after its package was uninstalled
// (ForgetMetrics) is not counted, so it leaves nothing behind.
func (h *Host) CountPushRows(id, kind string, n int) {
	h.mu.Lock()
	m := h.metrics[id]
	h.mu.Unlock()
	if m != nil {
		m.addPushRows(kind, n)
	}
}

// ForgetMetrics drops package id's counters; the bridge calls it when the
// package is uninstalled, so its series end with it.
func (h *Host) ForgetMetrics(id string) {
	h.mu.Lock()
	delete(h.metrics, id)
	h.mu.Unlock()
}

// MetricsRow is one installed package as the metrics report it: its state
// (a State, or a package state such as disabled or broken).
type MetricsRow struct {
	ID    string
	State string
}

// PackageStates are the package states beside State a MetricsRow may carry
// (pluginbridge reports them); gpuworks_plugin_state has a series for each
// and for each State.
var PackageStates = []string{"disabled", "installed", "broken", "revoked", "needs_host_setup"}

var processStates = []State{StateStarting, StateRunning, StateDegraded, StateBackoff, StateCrashloop,
	StateStopping, StateStopped, StateIdle, StateFailed}

// WriteMetrics writes the plugin metrics of rows, one package each, in the
// Prometheus text format (version 0.0.4). Only the packages in rows are
// written, so the label values are the bounded set of installed ids. A
// package's counters are zero until it is first started.
func (h *Host) WriteMetrics(w io.Writer, rows []MetricsRow) error {
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b MetricsRow) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	h.mu.Lock()
	ms := make([]*pluginMetrics, len(rows))
	for i, r := range rows {
		ms[i] = h.metrics[r.ID]
		if ms[i] == nil {
			ms[i] = newPluginMetrics()
		}
	}
	h.mu.Unlock()
	states := make([]string, 0, len(processStates)+len(PackageStates))
	for _, s := range processStates {
		states = append(states, string(s))
	}
	states = append(states, PackageStates...)
	slices.Sort(states)

	bw := bufio.NewWriter(w)
	// The buffered writer retains the first write error. Flush below returns
	// it whether it happened during a sample or during the final flush.
	family := func(name, typ, help string) {
		_, _ = bw.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " " + typ + "\n")
	}
	sample := func(name, labels string, v uint64) {
		_, _ = bw.WriteString(name + "{" + labels + "} " + strconv.FormatUint(v, 10) + "\n")
	}
	id := func(r MetricsRow) string { return `id="` + r.ID + `"` }

	family("gpuworks_plugin_up", "gauge", "Whether the plugin serves calls now (running or degraded).")
	for _, r := range rows {
		up := uint64(0)
		if State(r.State).Up() {
			up = 1
		}
		sample("gpuworks_plugin_up", id(r), up)
	}
	family("gpuworks_plugin_restarts_total", "counter", "Restarts of the plugin after it crashed or failed its health checks.")
	for i, r := range rows {
		sample("gpuworks_plugin_restarts_total", id(r), ms[i].restarts.Load())
	}
	family("gpuworks_plugin_state", "gauge", "The plugin's state: 1 for the state it is in, 0 for the others.")
	for _, r := range rows {
		for _, s := range states {
			v := uint64(0)
			if s == r.State {
				v = 1
			}
			sample("gpuworks_plugin_state", id(r)+`,state="`+s+`"`, v)
		}
	}
	family("gpuworks_plugin_calls_total", "counter", "Calls to the plugin by class and outcome.")
	for i, r := range rows {
		for c := range numClasses {
			for o := range numOutcomes {
				sample("gpuworks_plugin_calls_total", id(r)+`,class="`+classNames[c]+`",outcome="`+outcomeNames[o]+`"`, ms[i].calls[c][o].Load())
			}
		}
	}
	family("gpuworks_plugin_call_seconds", "histogram", "Time from a call's start until the plugin's response headers or the refusal, by class.")
	for i, r := range rows {
		for c := range numClasses {
			labels := id(r) + `,class="` + classNames[c] + `"`
			var cum uint64
			for b, bound := range callSecondsBounds {
				cum += ms[i].buckets[c][b].Load()
				sample("gpuworks_plugin_call_seconds_bucket", labels+`,le="`+strconv.FormatFloat(bound, 'g', -1, 64)+`"`, cum)
			}
			cum += ms[i].buckets[c][len(callSecondsBounds)].Load()
			sample("gpuworks_plugin_call_seconds_bucket", labels+`,le="+Inf"`, cum)
			_, _ = bw.WriteString("gpuworks_plugin_call_seconds_sum{" + labels + "} " +
				strconv.FormatFloat(time.Duration(ms[i].sumNanos[c].Load()).Seconds(), 'g', -1, 64) + "\n")
			sample("gpuworks_plugin_call_seconds_count", labels, cum)
		}
	}
	family("gpuworks_plugin_push_rows_total", "counter", "Entries of pushed tables the server applied, by table kind.")
	for i, r := range rows {
		for k, kind := range tableKinds {
			sample("gpuworks_plugin_push_rows_total", id(r)+`,kind="`+kind+`"`, ms[i].pushRows[k].Load())
		}
	}
	return bw.Flush()
}
