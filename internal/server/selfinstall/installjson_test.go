package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// legacyInstall is what writeInstallJSONInstall laid down.
type legacyInstall struct {
	DataDir string
	LogsDir string
}

// writeInstallJSONInstall lays down an installation made before server.yaml,
// as its install and upgrade left it: install.json, a systemd unit passing the
// settings as environment variables, the binary, and a database. The data and
// log directories are outside the install directory, so a test can tell that
// the layout came from install.json rather than from the defaults.
func (h *harness) writeInstallJSONInstall(releaseID, addr string) legacyInstall {
	h.t.Helper()
	root := h.t.TempDir()
	old := legacyInstall{DataDir: filepath.Join(root, "srv-data"), LogsDir: filepath.Join(root, "srv-logs")}
	writeFile(h.t, h.layout.Binary(), "old binary", 0o755)
	writeFile(h.t, filepath.Join(old.DataDir, "gpuworks.db"), "old database", 0o600)
	writeFile(h.t, filepath.Join(old.DataDir, "master.key"), "old key", 0o600)
	writeFile(h.t, h.layout.installJSONPath(), fmt.Sprintf(`{
  "version": 1,
  "release_id": %q,
  "dir": %q,
  "data_dir": %q,
  "logs_dir": %q,
  "addr": %q,
  "service_user": "gpuworks",
  "unit": "/etc/systemd/system/gpuworks.service",
  "mode": "systemd",
  "trusted_proxies": "127.0.0.1/32,::1/128",
  "log_max_size": "5MB",
  "log_keep": 7,
  "installed_at": "2026-09-10T14:45:40Z",
  "updated_at": "2026-09-28T23:04:01Z"
}
`, releaseID, h.layout.Dir, old.DataDir, old.LogsDir, addr), 0o644)
	writeFile(h.t, h.layout.UnitPath(), fmt.Sprintf(`[Unit]
Description=gpuworks control plane
Documentation=https://gpuworks.zstack.ai/
After=network-online.target
RequiresMountsFor=%[2]s

[Service]
Type=simple
User=gpuworks
Environment=GPUWORKS_ADDR=%[4]s
Environment=GPUWORKS_DATA_DIR=%[2]s
Environment=GPUWORKS_TRUSTED_PROXIES=127.0.0.1/32,::1/128
ExecStart=%[1]s serve --log-file %[3]s/gpuworks-server.log --log-max-size 5MB --log-keep 7
ExecStartPre=/usr/bin/test -x %[1]s
Restart=always
RestartSec=3
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`, h.layout.Binary(), old.DataDir, old.LogsDir, addr), 0o644)
	h.reportsRelease(releaseID)
	// The operator runs the installed binary, as `./gpuworks upgrade` in the
	// install directory does. useSite builds a new installer, so it comes first.
	h.installer.WithSelf(h.layout.Binary())
	return old
}

// assertAdopted checks that install.json's settings now live in server.yaml
// and that the unit starts the server from it.
func (h *harness) assertAdopted(old legacyInstall, console string) {
	h.t.Helper()
	cfg := h.config()
	if !slices.Equal(cfg.Listen.Console, []string{console}) || cfg.DataDir != old.DataDir ||
		cfg.Log.File != filepath.Join(old.LogsDir, "gpuworks-server.log") || cfg.Log.MaxSize != "5MB" || cfg.Log.Keep != 7 ||
		!slices.Equal(cfg.TrustedProxies, []string{"127.0.0.1/32", "::1/128"}) {
		h.t.Fatalf("server.yaml = %+v", cfg)
	}
	want := RenderUnit(UnitConfig{Binary: h.layout.Binary(), ConfigPath: h.layout.ConfigPath(), DataDir: old.DataDir, ServiceUser: DefaultServiceUser})
	if got := readFile(h.t, h.layout.UnitPath()); got != want {
		h.t.Fatalf("unit =\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(h.layout.installJSONPath()); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("install.json was left behind: %v", err)
	}
	inst, err := h.installer.readInstallation(context.Background())
	if err != nil || inst.FromInstallJSON || inst.Mode != ModeSystemd || inst.ServiceUser != DefaultServiceUser {
		h.t.Fatalf("installation reads back as %+v, %v", inst, err)
	}
}

// An installation made before server.yaml, whose binary an older release's
// upgrade already replaced, upgrades again without being adopted by hand.
func TestUpgradeMovesAnInstallJSONInstallationOntoServerYAML(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.useSite("v2.0.0", []byte("new binary"))
	addr := h.freeAddr()
	old := h.writeInstallJSONInstall("v1.0.0", addr)

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if report.From != "v1.0.0" || report.To != "v2.0.0" || report.Mode != ModeSystemd || report.RolledBack {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	// The address the unit recorded is the one server.yaml gets.
	h.assertAdopted(old, addr)
	// The new version is started from the rewritten unit.
	if !h.run.ranInOrder("sudo tee "+h.layout.UnitPath(), "sudo systemctl daemon-reload",
		"sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("the unit was not rewritten before the restart: %v", h.run.executed())
	}
	if !strings.Contains(h.out.String(), "moving the settings in "+h.layout.installJSONPath()) {
		t.Fatalf("the move was not reported: %s", h.out.String())
	}
	// The backup and the audit log are the ones in install.json's data
	// directory, and the backup holds the server.yaml a rollback starts from.
	members := readArchive(t, report.Backup)
	if members["data/gpuworks.db"] != "old database" || members["bin/gpuworks"] != "old binary" || members["server.yaml"] == "" {
		t.Fatalf("backup members = %v", slices.Sorted(maps.Keys(members)))
	}
	entries, err := ReadHistory(filepath.Join(old.DataDir, historyFile))
	if err != nil || len(entries) != 1 || entries[0].Result != "ok" || entries[0].ReleaseID != "v2.0.0" {
		t.Fatalf("history = %+v, %v", entries, err)
	}
}

// A rollback puts the previous binary back but keeps server.yaml and the
// rewritten unit: that binary is the one that wrote them, so it reads them.
func TestUpgradeRollbackKeepsAnInstallJSONInstallationOnServerYAML(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.useSite("v2.0.0", []byte("new binary"))
	addr := h.freeAddr()
	old := h.writeInstallJSONInstall("v1.0.0", addr)
	h.run.on = func(cmd Command) {
		if cmd.String() == "sudo systemctl restart gpuworks" && countCommands(h, "sudo systemctl restart gpuworks") == 1 {
			h.run.fail["sudo systemctl restart gpuworks"] = errors.New("exit status 1")
		}
	}

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !report.RolledBack {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if got := readFile(t, filepath.Join(old.DataDir, "gpuworks.db")); got != "old database" {
		t.Fatalf("database = %q", got)
	}
	h.assertAdopted(old, addr)
}

func TestUpgradeMovesANohupInstallJSONInstallationOntoServerYAML(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.useSite("v2.0.0", []byte("new binary"))
	h.writeInstallJSONInstall("v1.0.0", "127.0.0.1:8823")
	h.supervisedByNohup()

	if _, err := h.installer.Upgrade(context.Background(), h.upgradeReq()); err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if len(h.starts) != 1 || strings.Join(h.starts[0], " ") != h.layout.Binary()+" serve --supervise --config "+h.layout.ConfigPath() {
		t.Fatalf("detached starts = %v", h.starts)
	}
	if got := h.config().Listen.Console; !slices.Equal(got, []string{"127.0.0.1:8823"}) {
		t.Fatalf("listen = %v", got)
	}
	if h.run.ran("sudo systemctl") || h.run.ran("sudo tee") {
		t.Fatalf("a nohup installation got a unit: %v", h.run.executed())
	}
	if _, err := os.Stat(h.layout.installJSONPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install.json was left behind: %v", err)
	}
}

// Another binary may not move the installation: a rollback would restore the
// installed one, which may predate server.yaml and could not start from the
// rewritten unit.
func TestUpgradeRefusesToMoveAnInstallJSONInstallationFromAnotherBinary(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.useSite("v2.0.0", []byte("new binary"))
	old := h.writeInstallJSONInstall("v1.0.0", ":8800")
	unit := readFile(t, h.layout.UnitPath())
	h.installSelf("another binary")

	_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "predates server.yaml") ||
		!strings.Contains(err.Error(), "gpuworks install --dir "+h.layout.Dir) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(h.layout.ConfigPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("server.yaml was written: %v", err)
	}
	if readFile(t, h.layout.UnitPath()) != unit || readFile(t, h.layout.Binary()) != "old binary" {
		t.Fatal("the installation was changed")
	}
	if _, err := os.Stat(h.layout.installJSONPath()); err != nil {
		t.Fatalf("install.json: %v", err)
	}
	if h.run.ran("sudo systemctl") {
		t.Fatalf("the service was touched: %v", h.run.executed())
	}
	if _, err := os.Stat(filepath.Join(old.DataDir, historyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused upgrade was recorded: %v", err)
	}
}

// Checking for a release, or finding none, changes nothing.
func TestUpgradeCheckLeavesAnInstallJSONInstallationAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release string
		check   bool
	}{
		{"check", "v2.0.0", true},
		{"up to date", "v1.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.useSite(tc.release, []byte("new binary"))
			h.writeInstallJSONInstall("v1.0.0", ":8800")
			req := h.upgradeReq()
			req.Check = tc.check

			report, err := h.installer.Upgrade(context.Background(), req)
			if err != nil || report.Checked != tc.check || report.UpToDate == tc.check {
				t.Fatalf("report = %+v, err = %v", report, err)
			}
			if _, err := os.Stat(h.layout.ConfigPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("server.yaml was written: %v", err)
			}
			if _, err := os.Stat(h.layout.installJSONPath()); err != nil {
				t.Fatalf("install.json: %v", err)
			}
		})
	}
}

// Re-running install keeps what install.json recorded instead of the fresh
// install defaults (the default route's address, the default directories).
func TestInstallKeepsTheSettingsOfAnInstallJSONInstallation(t *testing.T) {
	h := newHarness(t, t.TempDir())
	addr := h.freeAddr()
	old := h.writeInstallJSONInstall("v1.0.0", addr)
	h.installSelf("binary v2")
	req := h.quiet()
	req.Addr, req.AddrSet = "", false

	report := h.mustInstall(req)
	if !report.Adopted || report.PasswordSource != PasswordUnchanged {
		t.Fatalf("report = %+v", report)
	}
	h.assertAdopted(old, addr)
	if got := readFile(t, filepath.Join(old.DataDir, "gpuworks.db")); got != "old database" {
		t.Fatalf("database = %q", got)
	}
}

func TestRestartMovesAnInstallJSONInstallationOntoServerYAML(t *testing.T) {
	h := newHarness(t, t.TempDir())
	addr := h.freeAddr()
	old := h.writeInstallJSONInstall("v1.0.0", addr)
	mode, err := h.installer.Restart(context.Background())
	if err != nil || mode != ModeSystemd {
		t.Fatalf("restart = %q, %v", mode, err)
	}
	h.assertAdopted(old, addr)
	if !h.run.ranInOrder("sudo systemctl daemon-reload", "sudo systemctl restart gpuworks") {
		t.Fatalf("the rewritten unit was not what restarted: %v", h.run.executed())
	}
}

func TestReadInstallationFromInstallJSON(t *testing.T) {
	h := newHarness(t, t.TempDir())
	old := h.writeInstallJSONInstall("v1.0.0", ":8800")
	inst, err := h.installer.readInstallation(context.Background())
	if err != nil || !inst.FromInstallJSON || inst.Mode != ModeSystemd || inst.ServiceUser != DefaultServiceUser ||
		inst.ReleaseID != "v1.0.0" || inst.Config.DataDir != old.DataDir {
		t.Fatalf("installation = %+v, %v", inst, err)
	}

	// server.yaml wins over an install.json left beside it.
	h.writeConfig(inst.Config)
	if inst, err := h.installer.readInstallation(context.Background()); err != nil || inst.FromInstallJSON {
		t.Fatalf("installation = %+v, %v", inst, err)
	}
	if err := os.Remove(h.layout.ConfigPath()); err != nil {
		t.Fatal(err)
	}

	// A missing address is the port those installations used by default.
	writeFile(t, h.layout.installJSONPath(), `{"version":1,"data_dir":"/srv/data"}`, 0o644)
	if inst, err := h.installer.readInstallation(context.Background()); err != nil ||
		!slices.Equal(inst.Config.Listen.Console, []string{"0.0.0.0:8800"}) || inst.Config.Log.File != "" {
		t.Fatalf("installation = %+v, %v", inst, err)
	}

	for _, damaged := range []string{`{"addr": `, `{"addr": "example.com:8800"}`} {
		writeFile(t, h.layout.installJSONPath(), damaged, 0o644)
		if _, err := h.installer.readInstallation(context.Background()); err == nil || errors.Is(err, ErrNotInstalled) ||
			!strings.Contains(err.Error(), h.layout.installJSONPath()) {
			t.Fatalf("%s: err = %v", damaged, err)
		}
	}
}
