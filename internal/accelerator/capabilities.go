package accelerator

import "strings"

// Optional backend capabilities. A backend implements one of these interfaces
// only when the capability applies; every consumer goes through the helper
// below, so a backend without the capability never needs a stub method and
// common code never branches on backend identity.

// RuntimeConstrainer is implemented by backends that can only run under
// specific container runtime drivers.
type RuntimeConstrainer interface {
	// SupportedRuntimes lists the runtime drivers the backend is verified
	// under, in the order they are named to an operator.
	SupportedRuntimes() []string
}

// ProbeExempt is implemented by backends that rely on embedded assets and
// admission checks instead of the behavioral partition probe.
type ProbeExempt interface {
	SkipsBehaviorProbe() bool
}

// HostRequirementNotifier is implemented by backends whose inference images
// must meet extra host-level requirements surfaced as capability notes.
type HostRequirementNotifier interface {
	HostRequirementNotes() []string
}

// LiveMemoryLimiter is implemented by backends whose memory limit can be
// changed on a running instance: the interposer reads it from state the
// agent can rewrite, not only from the environment the container started
// with.
type LiveMemoryLimiter interface {
	LiveMemoryLimit() bool
}

// LiveMemoryLimit reports whether a running instance of the backend can be
// held to a new memory limit without a restart.
func LiveMemoryLimit(b Backend) bool {
	if l, ok := b.(LiveMemoryLimiter); ok {
		return l.LiveMemoryLimit()
	}
	return false
}

// SliceUsageReporter is implemented by backends whose interposer keeps what
// a slice holds in a region the agent can read, so each slice's own use can
// be reported beside the card's.
type SliceUsageReporter interface {
	// SliceUsage reports which readings a slice running under the compute
	// limit smLimitPct has: the memory it is charged, and its compute
	// utilization.
	SliceUsage(smLimitPct int) (memory, compute bool)
}

// SliceUsage reports which of a slice's own readings the backend provides. A
// backend without the capability provides neither: the card's readings are
// all there is.
func SliceUsage(b Backend, smLimitPct int) (memory, compute bool) {
	if r, ok := b.(SliceUsageReporter); ok {
		return r.SliceUsage(smLimitPct)
	}
	return false, false
}

// RuntimeConstraint reports the runtime drivers the backend is limited to
// when driver is not one of them, or nil when the backend can run under
// driver. The caller turns the list into a reason code and its parameters.
func RuntimeConstraint(b Backend, driver string) []string {
	c, ok := b.(RuntimeConstrainer)
	if !ok {
		return nil
	}
	supported := c.SupportedRuntimes()
	driver = strings.ToLower(strings.TrimSpace(driver))
	for _, runtime := range supported {
		if runtime == driver {
			return nil
		}
	}
	return append([]string(nil), supported...)
}

// SkipsBehaviorProbe reports whether the backend is exempt from the
// behavioral partition probe.
func SkipsBehaviorProbe(b Backend) bool {
	if e, ok := b.(ProbeExempt); ok {
		return e.SkipsBehaviorProbe()
	}
	return false
}

// HostRequirementNotes returns extra host-level requirement notes for the
// backend, or nil when it has none.
func HostRequirementNotes(b Backend) []string {
	if n, ok := b.(HostRequirementNotifier); ok {
		return n.HostRequirementNotes()
	}
	return nil
}
