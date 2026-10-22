//go:build !linux

package fleet

import "testing"

// The fleet case measures a Linux control plane. On other platforms the
// descriptor limit and the resident set are read differently, and the numbers
// would not be comparable, so the case declines to run rather than report
// figures that cannot be compared with the ones CI collects.
func requireFileDescriptors(t *testing.T, want int) {
	t.Helper()
	t.Skipf("the fleet load case is measured on linux; this host cannot report the %d-descriptor budget comparably", want)
}

func processRSSMiB() float64 { return 0 }
