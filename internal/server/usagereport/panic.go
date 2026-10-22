package usagereport

import (
	"runtime"
	"strings"
)

// modulePrefix marks the platform's own functions in a stack.
const modulePrefix = "gpuworks.ai/gpuworks/"

// PanicSite names where a panic being recovered was raised: the innermost
// function of the platform itself below the panic, without the module
// prefix, file, line or arguments ("server/store.(*Store).CreateDeployment").
// It must be called from the deferred function that recovered, where the
// panicking frames are still on the stack.
func PanicSite() string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	panicking := false
	for {
		frame, more := frames.Next()
		switch {
		case frame.Function == "runtime.gopanic":
			panicking = true
		case panicking && strings.HasPrefix(frame.Function, modulePrefix):
			return strings.TrimPrefix(strings.TrimPrefix(frame.Function, modulePrefix), "internal/")
		}
		if !more {
			return Unknown
		}
	}
}
