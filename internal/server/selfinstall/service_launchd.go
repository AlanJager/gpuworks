package selfinstall

import (
	"context"
	"fmt"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/launchd"
)

// The launchd mode keeps the control plane as a launch daemon of the system
// domain (/Library/LaunchDaemons/ai.gpuworks.server.plist): launchd loads it at
// boot and starts it again when it exits. Every launchctl call on the system
// domain needs root, so each goes through sudo as the systemctl calls do.

const launchdDomain = "system"

func (s Service) launchdService() string { return launchdDomain + "/" + launchd.ServerLabel }

func (s Service) launchctl(ctx context.Context, args ...string) error {
	return s.Deps.Run.Run(ctx, Command{Path: "launchctl", Args: args, Sudo: true, Stderr: io.Discard})
}

// installPlist writes the plist. launchd refuses a system-domain plist that
// is not root's, so its owner is set explicitly whichever way it was written.
func (s Service) installPlist(ctx context.Context) error {
	path := s.Layout.LaunchdPath()
	if err := s.writeFile(ctx, path, RenderPlist(s.Cfg), 0o644); err != nil {
		return err
	}
	return s.Deps.Run.Run(ctx, Command{Path: "chown", Args: []string{"root:wheel", path}, Sudo: true})
}

// startLaunchd (re)loads the daemon from its plist, which may have just
// changed: unload what runs, wait for launchd to let go of it, and bootstrap
// the plist again, which starts it (RunAtLoad).
func (s Service) startLaunchd(ctx context.Context) error {
	if err := s.stopLaunchd(ctx); err != nil {
		return err
	}
	var err error
	// A bootstrap just after a bootout can still find the label held
	// ("Bootstrap failed: 5: Input/output error"); a few tries outlast it.
	for attempt := 0; attempt < 5; attempt++ {
		if err = s.launchctl(ctx, "bootstrap", launchdDomain, s.Layout.LaunchdPath()); err == nil {
			return nil
		}
		s.Deps.sleep(ctx, time.Second)
	}
	return fmt.Errorf("launchd did not load %s: %w", s.Layout.LaunchdPath(), err)
}

// stopLaunchd unloads the daemon and waits until launchd has let it go. The
// plist stays where it is, so the server comes back at the next boot or
// `gpuworks restart`.
func (s Service) stopLaunchd(ctx context.Context) error {
	if !s.launchdLoaded(ctx) {
		return nil
	}
	_ = s.launchctl(ctx, "bootout", s.launchdService())
	deadline := s.Deps.now().Add(s.Deps.stopTimeout())
	for s.launchdLoaded(ctx) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !s.Deps.now().Before(deadline) {
			return fmt.Errorf("launchd still holds %s after %s", s.launchdService(), s.Deps.stopTimeout())
		}
		s.Deps.sleep(ctx, 250*time.Millisecond)
	}
	return nil
}

// removePlist unloads the daemon and deletes its plist.
func (s Service) removePlist(ctx context.Context) error {
	if err := s.stopLaunchd(ctx); err != nil {
		return err
	}
	return s.Deps.Run.Run(ctx, Command{Path: "rm", Args: []string{"-f", s.Layout.LaunchdPath()}, Sudo: true})
}

// launchdLoaded reports whether launchd holds the daemon, running or between
// restarts.
func (s Service) launchdLoaded(ctx context.Context) bool {
	_, err := s.launchdPrint(ctx)
	return err == nil
}

// launchdPID is the pid of the running daemon, or 0.
func (s Service) launchdPID(ctx context.Context) int {
	out, err := s.launchdPrint(ctx)
	if err != nil {
		return 0
	}
	return launchd.RunningPID(out)
}

func (s Service) launchdPrint(ctx context.Context) (string, error) {
	if !s.Deps.launchdRunning() {
		return "", fmt.Errorf("launchd is not this host's init")
	}
	return s.Deps.Run.Output(ctx, Command{Path: "launchctl", Args: []string{"print", s.launchdService()}, Sudo: true, Stderr: io.Discard})
}
