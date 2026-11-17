//go:build darwin

package pluginexec

import (
	"fmt"
	"log/slog"
	"os"
)

// LandlockABI is 0: macOS has no Landlock.
func LandlockABI() int { return 0 }

// niceOf: x/sys returns the nice value itself on macOS.
func niceOf(raw int) int { return raw }

// prepare applies what macOS supports: a process group, resource limits
// and nice. There is no parent-death signal; the SDK polls getppid and
// exits on descriptor EOF instead.
func prepare(p *Profile, log *slog.Logger) error {
	if os.Getppid() != p.ParentPID {
		return fmt.Errorf("the server (pid %d) is gone", p.ParentPID)
	}
	if p.RequireLandlock {
		return fmt.Errorf("%w: macOS has no Landlock", errIsolation)
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
	if p.Landlock != nil {
		log.Warn("macOS has no Landlock; the plugin runs without a filesystem sandbox")
	}
	return nil
}
