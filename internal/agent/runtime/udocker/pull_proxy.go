package udocker

import (
	"context"
	"os"
	"os/exec"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func (d *Driver) cliPullCommand(ctx context.Context, cliImage string) *exec.Cmd {
	full := d.udockerInvocation([]string{"pull", cliImage})
	// A login profile can restore proxy variables after runuser starts. Enforce
	// the scoped choice inside that shell, passing credentials only in the
	// environment rather than exposing them in its command-line arguments.
	if d.runUser != "" && (runtime.PullDirect(ctx) || runtime.PullProxy(ctx) != "") {
		prefix := "unset HTTP_PROXY http_proxy HTTPS_PROXY https_proxy ALL_PROXY all_proxy NO_PROXY no_proxy; "
		if runtime.PullProxy(ctx) != "" {
			prefix += "export HTTP_PROXY=\"${GPUWORKS_PULL_PROXY:?missing task proxy}\" http_proxy=\"$GPUWORKS_PULL_PROXY\" HTTPS_PROXY=\"$GPUWORKS_PULL_PROXY\" https_proxy=\"$GPUWORKS_PULL_PROXY\"; unset GPUWORKS_PULL_PROXY; "
			if runtime.PullNoProxy(ctx) != "" {
				prefix += "export NO_PROXY=\"$GPUWORKS_PULL_NO_PROXY\" no_proxy=\"$GPUWORKS_PULL_NO_PROXY\"; unset GPUWORKS_PULL_NO_PROXY; "
			}
		}
		full[len(full)-1] = prefix + full[len(full)-1]
	}

	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	cmd.Dir = processDir
	if d.runUser == "" {
		cmd.Env = runtime.PullProxyEnv(ctx, d.cliEnv())
	} else {
		cmd.Env = runtime.PullProxyEnv(ctx, os.Environ())
		if proxy := runtime.PullProxy(ctx); proxy != "" {
			cmd.Env = append(cmd.Env, "GPUWORKS_PULL_PROXY="+proxy)
			if noProxy := runtime.PullNoProxy(ctx); noProxy != "" {
				cmd.Env = append(cmd.Env, "GPUWORKS_PULL_NO_PROXY="+noProxy)
			}
		}
	}
	return cmd
}
