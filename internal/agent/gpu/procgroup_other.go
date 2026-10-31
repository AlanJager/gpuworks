//go:build !unix

package gpu

import "os/exec"

// killGroupOnCancel leaves cmd as it is where process groups do not exist.
func killGroupOnCancel(cmd *exec.Cmd) {}
