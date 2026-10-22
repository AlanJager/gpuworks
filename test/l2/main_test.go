//go:build multiproc

package l2

import (
	"os"
	"testing"
)

// TestMain removes the directory the cached test binaries are built into.
// They are shared by every test in this package, so the directory cannot
// belong to any single one of them.
func TestMain(m *testing.M) {
	// The servers these tests start must not send usage reports.
	os.Setenv("GPUWORKS_USAGE_REPORT", "off")
	code := m.Run()
	buildMu.Lock()
	dir := buildRoot
	buildMu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
