package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Membership changes finish serially before this group starts. Each helper
// starts its own process and waits for login; starting both helpers allows the
// joined members to establish readiness together. Failure cancels the group,
// but every helper must return before the installer can finish.
func clusterInstallStartJoined(ctx context.Context, o clusterInstallOptions, action string, invoke func(context.Context, string, string, json.RawMessage) ([]byte, error), progress *clusterInstallProgress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hosts := o.Hosts[1:]
	results := make(chan error, len(hosts))
	for _, host := range hosts {
		go func() {
			_, err := invoke(ctx, host, action, nil)
			if err != nil {
				err = fmt.Errorf("start joined control plane on %s: %w", host, err)
			}
			results <- err
		}()
	}
	var failures []error
	detail := "并行启动临时控制面并等待登录"
	if action == "service" {
		detail = "并行由 systemd 启动从节点并等待登录"
	}
	for completed := 0; completed < len(hosts); completed++ {
		progress.Task(fmt.Sprintf("%s：%s（已返回 %d/%d）", detail, strings.Join(hosts, "、"), completed, len(hosts)))
		if err := <-results; err != nil {
			failures = append(failures, err)
			cancel()
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	return ctx.Err()
}
