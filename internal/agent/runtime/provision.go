package runtime

import (
	"errors"
	"time"
)

// ErrRuntimeUnavailable marks a failure of the runtime itself rather than of
// the operation asked of it: the runtime could not be set up on this node, so
// every operation fails the same way until the node changes. A caller that
// would otherwise try the next image source, or retry the next replica, tests
// for it with errors.Is and stops, because the next attempt fails identically.
var ErrRuntimeUnavailable = errors.New("runtime unavailable")

// UnavailableError is the error that wraps a runtime's provisioning failure.
// It matches ErrRuntimeUnavailable and unwraps to the cause.
type UnavailableError struct {
	Runtime string
	Err     error
}

func (e *UnavailableError) Error() string {
	return e.Runtime + " runtime unavailable: " + e.Err.Error()
}

func (e *UnavailableError) Unwrap() error { return e.Err }

func (e *UnavailableError) Is(target error) bool { return target == ErrRuntimeUnavailable }

// ProvisionState is what a runtime that sets itself up lazily (the udocker
// driver: run user, directories, patched PRoot) knows about that setup.
type ProvisionState struct {
	// Runtime is the runtime as operators know it, which may differ from the
	// driver's wire name (the "udocker" driver launches through runveil).
	Runtime string
	// Provisioned reports that the setup has completed. Before the first
	// attempt, and after a failed one, it is false.
	Provisioned bool
	// RunUser is the dedicated account workloads run as; empty means the
	// agent's own user. RunUID and RunGID are the identity either way.
	RunUser        string
	RunUID, RunGID int
	// FallbackReason is why workloads run as the agent's own user although the
	// agent could have used a dedicated one; empty when there was no fallback.
	FallbackReason string
	// LastError is the failure of the most recent setup attempt, empty when it
	// succeeded or none was made; LastErrorAt is when it happened.
	LastError   string
	LastErrorAt time.Time
	// RetryAt is when the next attempt may run after a failed one; callers
	// before then get the remembered failure.
	RetryAt time.Time
	// Probe says what an attempt does and Recheck is a command a person runs
	// on the node to look at what it depends on.
	Probe   string
	Recheck string
}

// ProvisionReporter is implemented by a runtime whose setup can fail. Reading
// the state never triggers the setup.
type ProvisionReporter interface {
	ProvisionState() ProvisionState
}
