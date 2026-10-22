//go:build multiproc

package l2

import (
	"os"
	"os/exec"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/installcred"
)

// serverCommand builds a `gpuworks serve` process whose first start creates
// the admin account with password, the way an installed server receives it:
// through <data>/initial-credentials, never the environment.
func serverCommand(t *testing.T, password, bin string, args ...string) *exec.Cmd {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--data" {
			if err := installcred.WriteInitialCredentials(args[i+1], "admin", password); err != nil {
				t.Fatalf("write initial credentials: %v", err)
			}
		}
	}
	cmd := exec.Command(bin, append(args, "--log-level", "warn")...)
	cmd.Env = os.Environ()
	return cmd
}
