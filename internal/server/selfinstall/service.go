package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Service supervises the installed control plane: through systemd when the
// host runs it, launchd on macOS, and a detached `serve --supervise` otherwise.
type Service struct {
	Layout Layout
	Mode   string
	Unit   string // unit path; empty means the standard systemd location
	Cfg    UnitConfig
	Deps   Deps
	IO     *IO
}

func (s Service) unitPath() string {
	if strings.TrimSpace(s.Unit) != "" {
		return s.Unit
	}
	return s.Layout.UnitPath()
}

// InstallUnit writes the service definition: the systemd unit (enabled), or
// the launch daemon plist, which launchd loads at boot from where it lies. It
// is called by install; upgrade and restart call it only to move an
// installation made before server.yaml onto it.
func (s Service) InstallUnit(ctx context.Context) error {
	switch s.Mode {
	case ModeLaunchd:
		return s.installPlist(ctx)
	case ModeSystemd:
	default:
		return nil
	}
	if err := s.writeFile(ctx, s.unitPath(), RenderUnit(s.Cfg), 0o644); err != nil {
		return err
	}
	if err := s.Deps.Run.Run(ctx, Command{Path: "systemctl", Args: []string{"daemon-reload"}, Sudo: true}); err != nil {
		return err
	}
	return s.Deps.Run.Run(ctx, Command{Path: "systemctl", Args: []string{"enable", ServiceName}, Sudo: true})
}

// RemoveUnit disables and deletes the unit or plist when an installation
// moves to another mode: its presence is what marks the mode.
func (s Service) RemoveUnit(ctx context.Context) error {
	if s.Mode == ModeLaunchd {
		return s.removePlist(ctx)
	}
	if err := s.Deps.Run.Run(ctx, Command{Path: "systemctl", Args: []string{"disable", ServiceName}, Sudo: true}); err != nil {
		return err
	}
	if err := s.Deps.Run.Run(ctx, Command{Path: "rm", Args: []string{"-f", s.unitPath()}, Sudo: true}); err != nil {
		return err
	}
	return s.Deps.Run.Run(ctx, Command{Path: "systemctl", Args: []string{"daemon-reload"}, Sudo: true})
}

// Start (re)starts the service, whether or not it is currently running.
func (s Service) Start(ctx context.Context) error {
	switch s.Mode {
	case ModeSystemd:
		return s.Deps.Run.Run(ctx, Command{Path: "systemctl", Args: []string{"restart", ServiceName}, Sudo: true})
	case ModeLaunchd:
		return s.startLaunchd(ctx)
	}
	if err := s.stopNohup(ctx); err != nil {
		return err
	}
	pid, err := s.Deps.StartDetached(ctx, Command{
		Path: s.Layout.Binary(),
		Args: supervisedServeArgs(s.Cfg),
		Dir:  s.Layout.Dir,
	})
	if err != nil {
		return fmt.Errorf("start %s: %w", s.Layout.Binary(), err)
	}
	return writePID(s.Layout.PIDFile(), pid)
}

// Stop shuts the service down and waits for it to exit.
func (s Service) Stop(ctx context.Context) error {
	switch s.Mode {
	case ModeSystemd:
		return s.Deps.Run.Run(ctx, Command{Path: "systemctl", Args: []string{"stop", ServiceName}, Sudo: true})
	case ModeLaunchd:
		return s.stopLaunchd(ctx)
	}
	return s.stopNohup(ctx)
}

// Active reports whether the service is running.
func (s Service) Active(ctx context.Context) bool {
	switch s.Mode {
	case ModeLaunchd:
		return s.launchdPID(ctx) > 0
	case ModeSystemd:
		return s.Deps.Run.Run(ctx, Command{Path: "systemctl", Args: []string{"is-active", "--quiet", ServiceName}, Sudo: true}) == nil
	}
	// The identity check stopping uses (a pid file left by a supervisor that
	// ran as a container's pid 1 names whatever pid 1 is now), except that a
	// server still loading its binary, argv not yet readable, is running.
	return mayBeOurServer(ctx, s.Deps.Run, readPID(s.Layout.PIDFile()), s.Layout.Binary())
}

// stopNohup terminates the detached process this layout started, if any.
func (s Service) stopNohup(ctx context.Context) error {
	pid := readPID(s.Layout.PIDFile())
	if pid == 0 {
		return nil
	}
	identity := identifyProcess(ctx, s.Deps.Run, pid, s.Layout.Binary())
	if identity == processUnknown {
		return fmt.Errorf("cannot identify live pid %d; the pid file was kept for retry", pid)
	}
	if identity != processOurs {
		// A recycled pid, or a process that is not the server: never signal it.
		_ = os.Remove(s.Layout.PIDFile())
		return nil
	}
	if pid == 1 {
		// `serve --supervise` as a container's main process: stopping it stops
		// the container, this command included, halfway through.
		return errors.New("the server is this container's main process (pid 1), and stopping it would stop the container; " +
			"upgrade it from the console, or restart the container")
	}
	if err := signalProcess(pid, "TERM"); err != nil {
		return fmt.Errorf("stop pid %d: %w", pid, err)
	}
	if !waitForExit(ctx, s.Deps, pid) {
		return fmt.Errorf("pid %d did not exit; the pid file was kept for retry", pid)
	}
	_ = os.Remove(s.Layout.PIDFile())
	return nil
}

// writeFile writes a file that may need root, reporting whether escalation
// happened through tee (which reads the content from stdin).
func (s Service) writeFile(ctx context.Context, path, content string, mode os.FileMode) error {
	if s.Deps.Euid == nil || s.Deps.Euid() == 0 {
		return writeFileAtomic(path, []byte(content), mode)
	}
	if err := mkdirAll(filepath.Dir(path), dirMode); err != nil {
		if err := s.Deps.Run.Run(ctx, Command{Path: "mkdir", Args: []string{"-p", filepath.Dir(path)}, Sudo: true}); err != nil {
			return err
		}
	}
	if err := s.Deps.Run.Run(ctx, Command{
		Path: "tee", Args: []string{path}, Sudo: true,
		Stdin: strings.NewReader(content), Stdout: io.Discard,
	}); err != nil {
		return err
	}
	return s.Deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{fmt.Sprintf("%04o", mode), path}, Sudo: true})
}
