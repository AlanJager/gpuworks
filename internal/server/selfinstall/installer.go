package selfinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Installer runs install and upgrade for one installation directory. Build it
// with New and use it for a single command: Install and Upgrade adopt the
// layout recorded by an existing installation, so the layout may change while
// the command runs and the type is not safe for concurrent use.
type Installer struct {
	layout Layout
	deps   Deps
	io     *IO
	// self is the executable to install; empty means the running one.
	self string
}

// New builds an installer for a layout.
func New(layout Layout, deps Deps, io *IO) *Installer {
	if io == nil {
		io = &IO{}
	}
	return &Installer{layout: layout, deps: deps, io: io}
}

// WithSelf overrides the executable install copies. Tests point it at a fixture
// binary instead of the test binary itself.
func (i *Installer) WithSelf(path string) *Installer {
	i.self = path
	return i
}

// selfPath resolves the binary install copies into place.
func (i *Installer) selfPath() (string, error) {
	if strings.TrimSpace(i.self) != "" {
		return i.self, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// unitConfig renders what the service definition needs.
func (i *Installer) unitConfig(serviceUser, mode string) UnitConfig {
	cfg := UnitConfig{
		Binary:      i.layout.Binary(),
		ConfigPath:  i.layout.ConfigPath(),
		DataDir:     i.layout.DataDir,
		ServiceUser: serviceUser,
	}
	cfg.StderrLog = i.layout.StderrLog()
	if mode == ModeNohup {
		// The detached process runs as whoever starts it.
		cfg.ServiceUser = ""
	}
	return cfg
}

// service builds the supervisor handle for a mode.
func (i *Installer) service(mode, serviceUser string, cfg UnitConfig) Service {
	return Service{
		Layout: i.layout,
		Mode:   mode,
		Cfg:    cfg,
		Deps:   i.deps,
		IO:     i.io,
	}
}

// serviceActive reports whether the installed service is currently running. It
// checks every supervision mode, because preflight must recognise an
// installation left behind by an earlier run with different flags, and is used
// to tell "the port is ours" from "the port belongs to something else".
func (i *Installer) serviceActive(ctx context.Context) bool {
	for _, mode := range []string{ModeSystemd, ModeLaunchd, ModeNohup} {
		if mode == ModeSystemd && !i.deps.systemdRunning() {
			// Only a running systemd can be asked (a Mac has no systemctl).
			continue
		}
		if i.service(mode, "", i.unitConfig("", mode)).Active(ctx) {
			return true
		}
	}
	return false
}
