package testenv

import (
	"path/filepath"
	"testing"

	serverapp "gpuworks.ai/gpuworks/internal/server/app"
)

// NewOnDiskConfigured is NewOnDisk with configure applied to the server's
// configuration before the server is built, for a load case that also
// needs, say, installable plugins switched on.
func NewOnDiskConfigured(t testing.TB, configure func(*serverapp.Config)) *Env {
	t.Helper()
	return newEnvConfigured(t, configure, "", filepath.Join(t.TempDir(), "gpuworks.db"), false)
}
