//go:build multiproc && !linux

package l2

import "testing"

// Elsewhere the descriptor limit and the resident-set reading work differently
// enough that the numbers could not be compared with a Linux run, so the case
// declines to run rather than report figures nobody can line up.

func readProcessRSSMiB(int) float64 { return 0 }

func readProcessCPU(int) float64 { return 0 }

func requireFleetFileDescriptors(t *testing.T, _ int) {
	t.Helper()
	t.Skip("the fleet load case reports Linux descriptor and memory figures; run it on Linux")
}

func readListenOverflows() int64 { return -1 }
