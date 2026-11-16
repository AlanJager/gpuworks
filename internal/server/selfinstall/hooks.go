package selfinstall

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

// InstallHook is a host step a separately built server adds to install,
// upgrade and uninstall: a udev rule and a group for a device the service must
// open, or a file only root can produce that the service later reads. A
// community build has none.
//
// The hooks run in the order given (Installer.WithHooks):
//
//   - install runs every Install once the service account exists and
//     server.yaml is written, before the service definition is written and
//     the service is started. A failure stops the install there; the hooks
//     before it are not undone, and running install again repeats them.
//   - upgrade runs every Install again after the new release is staged and
//     before the service is stopped, so a step this release changed is
//     applied before it starts; with hooks, a systemd unit is written again
//     too (its SupplementaryGroups). A failure stops the upgrade with the old
//     binary still installed. These are the hooks of the binary running the
//     upgrade, not of the release it installs.
//   - uninstall runs every Uninstall in reverse order once the service is
//     stopped and its definition removed, before any file is deleted. A
//     failure stops the uninstall; running it again repeats them.
//
// So both methods must be idempotent. They act on the host through run, which
// escalates a Command with Sudo, and must not assume they run as root.
// serviceUser is the account the service runs as; it is empty when the
// service runs as whoever starts it (the detached mode).
type InstallHook interface {
	Install(ctx context.Context, run Runner, layout Layout, serviceUser string) error
	Uninstall(ctx context.Context, run Runner, layout Layout) error
}

// ServiceGroups is implemented by an InstallHook whose steps need the
// service account in extra groups (a device group it created). Under systemd
// they are the unit's SupplementaryGroups=, so nothing changes the account
// itself; the hook's Install creates the groups before the unit is started.
type ServiceGroups interface {
	ServiceGroups() []string
}

// WithHooks sets the install hooks the installer runs (see InstallHook).
func (i *Installer) WithHooks(hooks ...InstallHook) *Installer {
	i.hooks = append([]InstallHook(nil), hooks...)
	return i
}

// runInstallHooks runs every hook's Install in order, stopping at the first
// failure.
func (i *Installer) runInstallHooks(ctx context.Context, serviceUser string) error {
	for n, hook := range i.hooks {
		if err := hook.Install(ctx, i.deps.Run, i.layout, serviceUser); err != nil {
			return fmt.Errorf("install step %d (%T): %w", n+1, hook, err)
		}
	}
	return nil
}

// runUninstallHooks runs every hook's Uninstall in reverse order, stopping at
// the first failure.
func (i *Installer) runUninstallHooks(ctx context.Context) error {
	for n := len(i.hooks) - 1; n >= 0; n-- {
		if err := i.hooks[n].Uninstall(ctx, i.deps.Run, i.layout); err != nil {
			return fmt.Errorf("undo install step %d (%T): %w", n+1, i.hooks[n], err)
		}
	}
	return nil
}

// reapplyInstallHooks is upgrade's part: the hooks again, and the systemd
// unit rewritten so its groups match them. Without hooks it does nothing, so
// an upgrade without them leaves the unit as install wrote it.
func (i *Installer) reapplyInstallHooks(ctx context.Context, svc Service) error {
	if len(i.hooks) == 0 {
		return nil
	}
	if err := i.runInstallHooks(ctx, svc.Cfg.ServiceUser); err != nil {
		return err
	}
	if svc.Mode != ModeSystemd {
		return nil
	}
	return svc.InstallUnit(ctx)
}

// supplementaryGroups is the groups the hooks ask for, each once, in order.
func (i *Installer) supplementaryGroups() []string {
	var out []string
	seen := map[string]bool{}
	for _, hook := range i.hooks {
		grouper, ok := hook.(ServiceGroups)
		if !ok {
			continue
		}
		for _, group := range grouper.ServiceGroups() {
			if group = strings.TrimSpace(group); group != "" && !seen[group] {
				seen[group] = true
				out = append(out, group)
			}
		}
	}
	return out
}

var dropInName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// DropInPath is where the systemd drop-in name for the server's unit lives:
// <unit dir>/gpuworks.service.d/<name>.conf.
func (l Layout) DropInPath(name string) string {
	return filepath.Join(l.UnitDir, ServiceName+".service.d", name+".conf")
}

// WriteDropIn writes the systemd drop-in name (lowercase letters, digits, "-"
// and "_") for the server's unit through run, escalating as needed. systemd
// reads it at the next daemon-reload, which install and upgrade do after the
// hooks when they write the unit.
func WriteDropIn(ctx context.Context, run Runner, layout Layout, name, content string) error {
	if !dropInName.MatchString(name) {
		return fmt.Errorf("drop-in name %q must be lowercase letters, digits, - and _", name)
	}
	path := layout.DropInPath(name)
	if err := run.Run(ctx, Command{Path: "mkdir", Args: []string{"-p", filepath.Dir(path)}, Sudo: true}); err != nil {
		return err
	}
	if err := run.Run(ctx, Command{Path: "tee", Args: []string{path}, Sudo: true, Stdin: strings.NewReader(content), Stdout: io.Discard}); err != nil {
		return err
	}
	return run.Run(ctx, Command{Path: "chmod", Args: []string{"0644", path}, Sudo: true})
}

// RemoveDropIn deletes the drop-in name; a missing one is not an error.
func RemoveDropIn(ctx context.Context, run Runner, layout Layout, name string) error {
	if !dropInName.MatchString(name) {
		return fmt.Errorf("drop-in name %q must be lowercase letters, digits, - and _", name)
	}
	return run.Run(ctx, Command{Path: "rm", Args: []string{"-f", layout.DropInPath(name)}, Sudo: true})
}
