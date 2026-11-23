package runtime

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TeardownError is a driver's account of a teardown it could not finish, as a
// code rather than a sentence (ADR 0067): Code is the stable fact a client
// renders, the wrapped error is the English fallback for logs, and nothing
// matches on its text. A driver returns one where it knows what was left
// behind; any other error reads as ErrorContainerRemovalPending.
type TeardownError struct {
	// Code is a proto instance runtime failure code.
	Code string
	// Container is the workload the teardown was for.
	Container string
	// Err is what the driver saw.
	Err error
}

func (e *TeardownError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Err.Error()
}

func (e *TeardownError) Unwrap() error { return e.Err }

// TeardownFailureCode is the code of a failed teardown: the one the driver
// stated, or ErrorContainerRemovalPending for an error that states none, so
// every pending teardown reaches the control plane as a code.
func TeardownFailureCode(err error) string {
	var teardown *TeardownError
	if errors.As(err, &teardown) && teardown.Code != "" {
		return teardown.Code
	}
	return proto.ErrorContainerRemovalPending
}
