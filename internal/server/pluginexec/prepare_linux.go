//go:build linux

package pluginexec

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"
)

// landlockABI is LandlockABI; tests of the refusal path replace it.
var landlockABI = LandlockABI

// niceOf converts the raw getpriority answer (20 - nice) to a nice value.
func niceOf(raw int) int { return 20 - raw }

// prepare applies p to the calling thread, which then executes the plugin.
// The order matters: the parent-death signal first, so nothing below runs
// as an orphan; Landlock last, since it removes access the earlier steps
// need (cgroupfs, /dev/null).
func prepare(p *Profile, log *slog.Logger) error {
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return fmt.Errorf("parent-death signal: %w", err)
	}
	if ppid := os.Getppid(); ppid != p.ParentPID {
		return fmt.Errorf("the server (pid %d) is gone; the parent is now %d", p.ParentPID, ppid)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := ownGroup(); err != nil {
		return fmt.Errorf("process group: %w", err)
	}
	if err := checkDescriptors(); err != nil {
		return err
	}
	if err := setLimits(p); err != nil {
		return err
	}
	if err := os.Chdir(p.Dir); err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	if p.Cgroup != nil {
		moved, warnings, err := MoveToCgroup(p.Cgroup, p.ID, os.Getpid())
		for _, w := range warnings {
			log.Warn("cgroup limit not applied", "detail", w)
		}
		switch {
		case err != nil:
			log.Warn("the plugin runs outside a cgroup of its own", "err", err)
		case !moved:
			log.Info("the server's cgroup is not delegated; the plugin shares it")
		}
	}
	return sandbox(p, log)
}

// sandbox applies the Landlock ruleset, or refuses when the profile
// requires one and the kernel has none.
func sandbox(p *Profile, log *slog.Logger) error {
	abi := landlockABI()
	switch {
	case abi == 0 && p.RequireLandlock:
		return fmt.Errorf("%w: this kernel has no Landlock", errIsolation)
	case abi == 0:
		if p.Landlock != nil {
			log.Warn("this kernel has no Landlock; the plugin runs without a filesystem sandbox")
		}
		return nil
	case p.Landlock == nil && p.RequireLandlock:
		return errors.New("the profile requires Landlock but lists no ruleset")
	case p.Landlock == nil:
		return nil
	}
	return applyLandlock(p.Landlock, abi)
}
