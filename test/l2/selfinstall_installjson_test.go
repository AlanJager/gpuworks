//go:build multiproc

package l2

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// An installation made before server.yaml kept its settings in install.json
// and passed them to the server on its command line. The older release's
// upgrade replaced its binary with one that reads server.yaml but left that
// layout alone, so the next upgrade, run by the new binary, finds no
// server.yaml. It must still upgrade — moving the settings over on the way —
// instead of asking the operator to adopt the installation by hand.
func TestUpgradeMovesAnInstallJSONInstallationOntoServerYAML(t *testing.T) {
	site := newReleaseSite(t)
	site.publish(t, "v1.2.4", buildServerRelease(t, "v1.2.4"))
	fx := installControlPlane(t, "v1.2.3")
	addr := strings.TrimPrefix(fx.base, "http://")
	logFile := filepath.Join(fx.dir, "logs", "gpuworks-server.log")

	// Put the installation back into the layout the older release's install
	// and upgrade left: install.json instead of server.yaml, and the server
	// started the way that release's nohup fallback started it.
	stopInstalledControlPlane(t, fx.dir)
	if err := os.Remove(filepath.Join(fx.dir, "server.yaml")); err != nil {
		t.Fatal(err)
	}
	installJSON := fmt.Sprintf(`{
  "version": 1,
  "release_id": "v1.2.3",
  "dir": %q,
  "data_dir": %q,
  "logs_dir": %q,
  "addr": %q,
  "service_user": "",
  "mode": "nohup",
  "trusted_proxies": "127.0.0.1/32,::1/128",
  "log_max_size": "10MB",
  "log_keep": 20,
  "installed_at": "2026-09-10T14:45:40Z"
}
`, fx.dir, fx.dataDir, filepath.Dir(logFile), addr)
	if err := os.WriteFile(filepath.Join(fx.dir, "install.json"), []byte(installJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	startLegacyServer(t, fx, "serve", "--addr", addr, "--data", fx.dataDir, "--log-file", logFile,
		"--trusted-proxies", "127.0.0.1/32,::1/128", "--log-max-size", "10MB", "--log-keep", "20")
	assertHealthVersion(t, fx.base, "v1.2.3")

	out := runTool(t, fx.bin, "upgrade", "--dir", fx.dir, "--base-url", site.URL)
	if !strings.Contains(out, "moving the settings in "+filepath.Join(fx.dir, "install.json")) ||
		!strings.Contains(out, "upgraded v1.2.3 -> v1.2.4") {
		t.Fatalf("upgrade output:\n%s", out)
	}
	assertHealthVersion(t, fx.base, "v1.2.4")

	cfg := readInstalledConfig(t, fx.dir)
	if len(cfg.Listen.Console) != 1 || cfg.Listen.Console[0] != addr || cfg.DataDir != fx.dataDir || cfg.Log.File != logFile {
		t.Fatalf("server.yaml = %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "install.json")); !os.IsNotExist(err) {
		t.Fatalf("install.json was left behind: %v", err)
	}
	entries := readInstalledHistory(t, fx.dir)
	if last := entries[len(entries)-1]; last.Action != "upgrade" || last.Result != "ok" ||
		last.ReleaseID != "v1.2.4" || last.PreviousReleaseID != "v1.2.3" {
		t.Fatalf("the upgrade was not recorded: %+v", last)
	}
	// The new release runs from server.yaml behind the supervisor, not from the
	// old command line.
	pid := readPidFile(t, filepath.Join(fx.dir, "gpuworks.pid"))
	if argv := cmdlineOf(t, pid); strings.Join(argv[1:], " ") != "serve --supervise --config "+filepath.Join(fx.dir, "server.yaml") {
		t.Fatalf("pid %d runs %q", pid, argv)
	}
}

// startLegacyServer starts the installed binary detached, with the pid file
// the nohup fallback keeps, so the upgrade stops it as it would its own.
func startLegacyServer(t *testing.T, fx controlPlane, args ...string) {
	t.Helper()
	cmd := exec.Command(fx.bin, args...)
	cmd.Dir = fx.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", fx.bin, err)
	}
	// Reap it, so that once the upgrade stops it the pid is gone rather than
	// a zombie the upgrade would wait on.
	go func() { _ = cmd.Wait() }()
	if err := os.WriteFile(filepath.Join(fx.dir, "gpuworks.pid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o664); err != nil {
		t.Fatal(err)
	}
}
