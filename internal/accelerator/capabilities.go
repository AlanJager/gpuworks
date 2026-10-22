package accelerator

// Optional backend capabilities. A backend implements one of these interfaces
// only when the capability applies; every consumer goes through the helper
// below, so a backend without the capability never needs a stub method and
// common code never branches on backend identity.

// RuntimeConstrainer is implemented by backends that can only run under
// specific container runtime drivers.
type RuntimeConstrainer interface {
	// RuntimeConstraint returns a human-readable reason the backend cannot
	// run under the given runtime driver, or "" when it can.
	RuntimeConstraint(driver string) string
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

// RuntimeConstraint reports why the backend cannot run under the given
// container runtime driver, or "" when it can.
func RuntimeConstraint(b Backend, driver string) string {
	if c, ok := b.(RuntimeConstrainer); ok {
		return c.RuntimeConstraint(driver)
	}
	return ""
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
