//go:build !unix

package gpu

import "os/exec"

// killGroupOnCancel kills only the process itself where process groups do
// not exist.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
