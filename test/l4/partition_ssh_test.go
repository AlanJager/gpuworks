//go:build cloudgpu && partition

package l4

import (
	"context"
	"os/exec"
	"strings"
)

// partitionSSHCommand centralizes the non-interactive SSH contract used by
// partition hardware tests. jump is optional and deliberately comes from an
// environment variable so private topology never enters the repository.
func partitionSSHCommand(ctx context.Context, host, jump, command string) *exec.Cmd {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=8"}
	if jump = strings.TrimSpace(jump); jump != "" {
		args = append(args, "-o", "ProxyJump="+jump)
	}
	args = append(args, host, command)
	return exec.CommandContext(ctx, "ssh", args...)
}

func partitionSCPCommand(ctx context.Context, jump string, sources []string, destination string) *exec.Cmd {
	args := []string{"-q", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8"}
	if jump = strings.TrimSpace(jump); jump != "" {
		args = append(args, "-o", "ProxyJump="+jump)
	}
	args = append(args, sources...)
	args = append(args, destination)
	return exec.CommandContext(ctx, "scp", args...)
}
