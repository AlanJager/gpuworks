//go:build !linux && !darwin

package selfinstall

import "context"

// Supervise is not available on this platform.
func Supervise(context.Context, SuperviseOptions) error { return errSuperviseUnsupported }
