package selfinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// installJSONFile is where installations made before server.yaml recorded
// their settings, and where they still are until a command moves them over.
// Nothing writes it any more.
const installJSONFile = "install.json"

// installJSON is the part of install.json that became server.yaml. The
// service mode, service user and release it also recorded are read from the
// machine, as for any other installation.
type installJSON struct {
	DataDir        string `json:"data_dir"`
	LogsDir        string `json:"logs_dir"`
	Addr           string `json:"addr"`
	TrustedProxies string `json:"trusted_proxies"`
	LogMaxSize     string `json:"log_max_size"`
	LogKeep        int    `json:"log_keep"`
}

func (l Layout) installJSONPath() string { return filepath.Join(l.Dir, installJSONFile) }

// readInstallJSON is the server.yaml an install.json stands for. A damaged
// file is an error: the layout it describes cannot be guessed.
func readInstallJSON(path string) (serverconfig.File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return serverconfig.File{}, err
	}
	var old installJSON
	if err := json.Unmarshal(raw, &old); err != nil {
		return serverconfig.File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	addr := strings.TrimSpace(old.Addr)
	if addr == "" {
		// What those installations served on when no address was given.
		addr = ":" + serverconfig.DefaultPort
	}
	listen, err := listenForAddr(addr)
	if err != nil {
		return serverconfig.File{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg := serverconfig.File{
		Listen:         listen,
		DataDir:        strings.TrimSpace(old.DataDir),
		Log:            serverconfig.Log{MaxSize: strings.TrimSpace(old.LogMaxSize), Keep: old.LogKeep},
		TrustedProxies: splitList(old.TrustedProxies),
	}
	if logs := strings.TrimSpace(old.LogsDir); logs != "" {
		cfg.Log.File = Layout{LogsDir: logs}.LogFile()
	}
	return cfg, nil
}

// adoptInstallJSON moves an installation made before server.yaml onto it, so
// whatever starts next — the new release, or this binary after a rollback —
// starts the way install starts it: the settings install.json recorded become
// server.yaml, a unit that passed them as environment variables is rewritten
// to `serve --config`, and install.json is removed.
//
// Only the installed binary may do this. A rollback puts that binary back, and
// one older than server.yaml would not start from the rewritten unit.
func (i *Installer) adoptInstallJSON(ctx context.Context, inst installation, svc Service) error {
	if !i.runsInstalledBinary() {
		return fmt.Errorf("%s predates server.yaml and only its own binary can move it over: run this command as %s, or adopt the installation with `gpuworks install --dir %s`",
			i.layout.Dir, i.layout.Binary(), i.layout.Dir)
	}
	cfg := inst.Config
	cfg.DataDir = i.layout.DataDir
	cfg.Log.File = i.layout.LogFile()
	payload, err := serverconfig.Render(cfg)
	if err != nil {
		return err
	}
	i.io.Say("moving the settings in %s to %s", i.layout.installJSONPath(), i.layout.ConfigPath())
	if err := i.writeInstalled(ctx, i.layout.ConfigPath(), payload, 0o664); err != nil {
		return fmt.Errorf("write %s: %w", i.layout.ConfigPath(), err)
	}
	if err := svc.InstallUnit(ctx); err != nil {
		// Without server.yaml the installation still reads as one to adopt,
		// so the next run tries again instead of starting from a stale unit.
		_ = os.Remove(i.layout.ConfigPath())
		return err
	}
	i.removeInstallJSON(ctx)
	return nil
}

// runsInstalledBinary reports whether this process is the installed binary.
func (i *Installer) runsInstalledBinary() bool {
	self, err := i.selfPath()
	if err != nil {
		return false
	}
	a, errA := os.Stat(self)
	b, errB := os.Stat(i.layout.Binary())
	return errA == nil && errB == nil && os.SameFile(a, b)
}

// removeInstallJSON deletes install.json once server.yaml replaces it. A copy
// left behind is only untidy, since server.yaml wins whenever both exist, so a
// failure is a warning.
func (i *Installer) removeInstallJSON(ctx context.Context) {
	path := i.layout.installJSONPath()
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) && i.canSudo() {
		err = i.deps.Run.Run(ctx, Command{Path: "rm", Args: []string{"-f", path}, Sudo: true})
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		i.io.Warn("warning: could not remove %s: %v", path, err)
	}
}
