package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// superviseServe runs `serve --supervise`: this process stays in front and
// keeps `serve` (the same arguments without the flag) running, so a console
// upgrade or a console restart can exit and be started again where there is
// no systemd. Started from an installation's server.yaml with its binary, it
// records itself as that installation's process.
func superviseServe(log *slog.Logger, configPath string, args []string) error {
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the server binary: %w", err)
	}
	opts := selfinstall.SuperviseOptions{
		Binary: binary,
		Args:   append([]string{"serve"}, selfinstall.WithoutSuperviseFlag(args)...),
		Logf:   func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	}
	if configPath != "" {
		if abs, err := filepath.Abs(configPath); err == nil {
			layout := selfinstall.NewLayout(filepath.Dir(abs), "", "")
			if sameExecutable(binary, layout.Binary()) {
				opts.PIDFile = layout.PIDFile()
			}
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return selfinstall.Supervise(ctx, opts)
}

func sameExecutable(a, b string) bool {
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}
