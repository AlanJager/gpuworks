package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// The connector service: `expose run` is the process, `up` starts it in the
// background or as a systemd unit, and `down` makes sure it is gone.

// exposeRun is the connector service itself. It hosts the entry point until
// the entry point is disabled, and it is the only process that owns the
// connector.
func exposeRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("expose run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "", "data directory (default: discovered)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, _ := expose.ResolveDataDir(*dataDir)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("connector service starting", "data", dir, "pid", os.Getpid())
	return expose.RunService(ctx, expose.NewStore(dir), log)
}

// ensureService makes sure a connector service is there to pick the request
// up: an existing one converges on the new configuration by itself.
func ensureService(ctx context.Context, io *selfinstall.IO, store *expose.Store, dataDirFlag string, systemd bool) error {
	dir, _ := expose.ResolveDataDir(dataDirFlag)
	abs, err := expose.AbsDataDir(dir)
	if err != nil {
		return err
	}
	installed := systemdServiceInstalled()
	if store.SupervisorHeld() {
		if systemd && !installed {
			io.Warn("the connector service already runs as a background process; to move it under systemd, run `gpuworks expose down` and then `gpuworks expose up --systemd`")
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if systemd || installed {
		return installSystemdService(ctx, io, exe, abs)
	}
	pid, err := expose.StartService(exe, store, abs)
	if err != nil {
		return fmt.Errorf("start the connector service: %w", err)
	}
	io.Say("started the connector service in the background (pid %d, log %s)", pid, store.DaemonLogPath())
	io.Say("it is not started again after a reboot; `gpuworks expose up --systemd` installs it as a service that is")
	return nil
}

// systemdServiceInstalled reports whether the connector service is a systemd
// unit on this host.
func systemdServiceInstalled() bool {
	_, err := os.Stat(expose.SystemdUnitPath)
	return err == nil
}

// installSystemdService writes the unit for this data dir and (re)starts it.
func installSystemdService(ctx context.Context, io *selfinstall.IO, exe, dataDir string) error {
	if runtime.GOOS != "linux" {
		return errors.New("--systemd is only available on Linux")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("--systemd writes %s: run it as root (sudo)", expose.SystemdUnitPath)
	}
	if raw, err := os.ReadFile(expose.SystemdUnitPath); err == nil && !strings.Contains(string(raw), dataDir) {
		return fmt.Errorf("%s already serves another data directory; run `gpuworks expose down` for that one first", expose.SystemdUnitName)
	}
	account := ""
	if u, err := expose.ServiceAccount(dataDir); err != nil {
		return err
	} else if u != nil {
		account = u.Username
	}
	unit, err := expose.RenderServiceUnit(exe, dataDir, account)
	if err != nil {
		return err
	}
	if err := os.WriteFile(expose.SystemdUnitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", expose.SystemdUnitName}, {"restart", expose.SystemdUnitName}} {
		if err := systemctl(ctx, args...); err != nil {
			return err
		}
	}
	io.Say("installed and started %s (it starts again after a reboot)", expose.SystemdUnitName)
	return nil
}

// removeSystemdService stops and removes the unit, if there is one.
func removeSystemdService(ctx context.Context, io *selfinstall.IO) error {
	if !systemdServiceInstalled() {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("the connector service is the systemd unit %s: run this as root (sudo) to remove it", expose.SystemdUnitName)
	}
	if err := systemctl(ctx, "disable", "--now", expose.SystemdUnitName); err != nil {
		return err
	}
	if err := os.Remove(expose.SystemdUnitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	io.Say("removed %s", expose.SystemdUnitName)
	return nil
}

func systemctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// serviceDescription says what runs the connector, for `status`.
func serviceDescription(store *expose.Store) string {
	running := store.SupervisorHeld()
	switch {
	case systemdServiceInstalled() && running:
		return "systemd unit " + expose.SystemdUnitName + " (running)"
	case systemdServiceInstalled():
		return "systemd unit " + expose.SystemdUnitName + " (not running)"
	case running:
		return "background process (not started again after a reboot)"
	default:
		return "not running"
	}
}
