package pluginhost

import (
	"maps"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// State is where a plugin's process stands.
type State string

const (
	StateStarting  State = "starting"  // spawned, handshake not yet accepted
	StateRunning   State = "running"   // handshake accepted, healthy
	StateDegraded  State = "degraded"  // running, its health names a problem
	StateBackoff   State = "backoff"   // crashed, waiting to restart
	StateCrashloop State = "crashloop" // crashed too often, retried slowly
	StateStopping  State = "stopping"  // asked to stop
	StateStopped   State = "stopped"   // stopped on request
	StateIdle      State = "idle"      // an on-demand plugin not running now
	StateFailed    State = "failed"    // will not run until started again
)

// Up reports whether the plugin serves calls. Every other state counts as
// down for its pushed state (ADR 0071, decision 9).
func (s State) Up() bool { return s == StateRunning || s == StateDegraded }

// Status is a plugin's supervision state.
type Status struct {
	ID    string
	State State
	// Code and Params say why the plugin is in State (a crash, a refusal,
	// the plugin's own degraded code); empty when nothing needs saying.
	Code   string
	Params map[string]any
	Since  time.Time
	PID    int
	// Starts counts the processes started since the plugin was added.
	Starts int
	// Generation numbers the current (or last) process: it rises with
	// every process the host starts, for any plugin, and is the
	// generation its host API connection pushes under (Owner.Generation).
	Generation uint64
	// Isolation is "landlock" when the plugin runs in a Landlock sandbox,
	// "none" otherwise.
	Isolation string
	// Use is the capability versions negotiated at the last handshake.
	Use map[string]int
	// Cgroup is the plugin's own cgroup, relative to the cgroup mount, when
	// its process runs there with a memory limit; empty when it shares the
	// server's cgroup and the RSS watchdog bounds it instead.
	Cgroup string
	// RSSBytes is the resident memory of the plugin's process group at the
	// watchdog's last sample; 0 before the first sample and whenever the
	// plugin runs in a cgroup of its own or not at all.
	RSSBytes int64
}

func (s Status) clone() Status {
	s.Params = maps.Clone(s.Params)
	s.Use = maps.Clone(s.Use)
	return s
}

// Session is what other parts of the server need of a running plugin.
type Session struct {
	// PrincipalKey is this process's key for signing the principal
	// headers on proxied routes; fresh per spawn.
	PrincipalKey []byte
	Use          map[string]int
	Services     []string
	Health       extapi.Health
}
