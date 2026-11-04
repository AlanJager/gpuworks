package selfinstall

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/installcred"
)

func TestInstallSystemdFresh(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("gpuworks binary v1")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	req.Password = "fresh-install-password-2026"

	report := h.mustInstall(req)

	if report.Mode != ModeSystemd {
		t.Fatalf("mode = %q, want systemd (stderr: %s)", report.Mode, h.errOut.String())
	}
	if report.Adopted {
		// A fresh layout must not claim adoption, no matter what another
		// installation left in the host's systemd directory.
		t.Fatalf("a fresh install reported itself adopted: %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "gpuworks binary v1" {
		t.Fatalf("installed binary = %q", got)
	}
	info, err := os.Stat(h.layout.Binary())
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	if info.Mode().Perm() != binaryMode {
		t.Fatalf("binary mode = %v, want %v", info.Mode().Perm(), binaryMode)
	}

	if !h.run.ran("sudo chmod -R g+rwX "+h.layout.Dir) ||
		!h.run.ran("sudo find "+h.layout.Dir+" -type d -exec chmod g+s {} +") {
		t.Fatalf("the tree was not opened as shared workspace: %v", h.run.executed())
	}
	// Once before the service starts, once at the very end as the safety net
	// for writes that went through the sudo fallbacks.
	if got := countCommands(h, "sudo chown -R "+DefaultServiceUser+":"+DefaultServiceUser+" "+h.layout.Dir); got != 2 {
		t.Fatalf("chowns = %d, want the mid-install one and the final safety net: %v", got, h.run.executed())
	}

	unit := h.unitFile()
	for _, want := range []string{
		"User=" + DefaultServiceUser,
		"RequiresMountsFor=" + h.layout.DataDir,
		"ExecStart=" + h.layout.Binary() + " serve --config " + h.layout.ConfigPath() + "\n",
		"ExecStartPre=/usr/bin/test -x " + h.layout.Binary(),
		"Restart=always",
		"TimeoutStopSec=30",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit file is missing %q:\n%s", want, unit)
		}
	}
	// The unit is world-readable, so the bootstrap password must never be in it.
	if strings.Contains(unit, req.Password) {
		t.Fatalf("the unit file contains the admin password:\n%s", unit)
	}
	// Every setting lives in server.yaml; the unit carries none.
	if strings.Contains(unit, "Environment=") || strings.Contains(unit, "GPUWORKS_") {
		t.Errorf("the unit carries settings:\n%s", unit)
	}
	if !h.run.ranInOrder("sudo systemctl daemon-reload", "sudo systemctl enable gpuworks", "sudo systemctl restart gpuworks") {
		t.Errorf("systemd was not driven in order: %v", h.run.executed())
	}

	policy, modeErr := h.installer.sensitiveFilePolicy()
	if modeErr != nil {
		t.Fatal(modeErr)
	}
	creds, err := installcred.ReadInitialCredentials(h.layout.DataDir, policy)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.User != DefaultAdminUser || creds.Password != req.Password {
		t.Fatalf("credentials = %+v", creds)
	}
	credInfo, err := os.Stat(h.layout.CredentialsPath())
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if credInfo.Mode().Perm() != policy.Mode() {
		t.Fatalf("credentials mode = %v, want 0660 (the server reads it as the service account)", credInfo.Mode().Perm())
	}

	cfg := h.config()
	if len(cfg.Listen.Console) != 1 || cfg.Listen.Console[0] != req.Addr || cfg.DataDir != h.layout.DataDir ||
		cfg.Log.File != h.layout.LogFile() || cfg.Log.MaxSize != DefaultLogMaxSize || cfg.Log.Keep != DefaultLogKeep ||
		strings.Join(cfg.TrustedProxies, ",") != DefaultTrustedProxies {
		t.Fatalf("server.yaml = %+v", cfg)
	}
	if info, err := os.Stat(h.layout.ConfigPath()); err != nil || info.Mode().Perm() != 0o664 {
		t.Fatalf("server.yaml must be group-writable so the console can save listen addresses: %v %v", info, err)
	}
	// The installation reads back from the machine as what was installed.
	inst, err := h.installer.readInstallation(context.Background())
	if err != nil || inst.Mode != ModeSystemd || inst.ServiceUser != DefaultServiceUser {
		t.Fatalf("installation = %+v, %v", inst, err)
	}
	if _, err := os.Stat(filepath.Join(h.layout.Dir, "install.json")); !os.IsNotExist(err) {
		t.Fatalf("install.json must not be written: %v", err)
	}

	entries, err := ReadHistory(h.layout.HistoryPath())
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != ActionInstall || entries[0].Result != "ok" {
		t.Fatalf("history = %+v", entries)
	}
	if report.PasswordSource != PasswordFromFlag || report.Credentials != h.layout.CredentialsPath() {
		t.Fatalf("report = %+v", report)
	}
	if want := "http://" + req.Addr + "/healthz"; report.HealthURL != want {
		t.Fatalf("health url = %q, want %q", report.HealthURL, want)
	}
}

func TestInstallCreatesServiceUserOnlyWhenMissing(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.users[DefaultServiceUser] = true
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser

	h.mustInstall(req)
	if h.run.ran("sudo useradd") || h.run.ran("sudo adduser") {
		t.Fatalf("an existing account must be reused: %v", h.run.executed())
	}
	if !h.run.ran("sudo chown -R " + DefaultServiceUser + ":" + DefaultCredentialGroup) {
		t.Fatalf("the install tree was not given to the service user: %v", h.run.executed())
	}

	h2 := newHarness(t, t.TempDir())
	h2.installSelf("binary")
	req2 := h2.quiet()
	req2.ServiceUser = DefaultServiceUser
	h2.mustInstall(req2)
	if !h2.run.ran("sudo useradd --system --gid " + DefaultCredentialGroup + " --home-dir " + h2.layout.Dir) {
		t.Fatalf("the service account was not created: %v", h2.run.executed())
	}
	if !strings.Contains(h2.unitFile(), "Group="+DefaultCredentialGroup+"\n") {
		t.Fatalf("new service does not use the explicitly authorized group: %s", h2.unitFile())
	}
}

func TestInstallFallsBackToAdduserOnBusybox(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	h.run.fail["sudo useradd"] = errors.New("exec: useradd: not found")

	h.mustInstall(req)
	if !h.run.ran("sudo adduser -S -D -H") {
		t.Fatalf("busybox adduser was not tried: %v", h.run.executed())
	}
}

func TestInstallReportsFailedUserCreation(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	h.run.fail["sudo useradd"] = errors.New("exit status 1")
	h.run.fail["sudo adduser"] = errors.New("exit status 1")

	if _, err := h.installer.Install(context.Background(), req); err == nil {
		t.Fatal("install succeeded although no service account could be created")
	}
	if h.run.ran("sudo systemctl daemon-reload") || h.run.ran("sudo systemctl restart gpuworks") {
		t.Fatalf("the service was driven although the account was never created: %v", h.run.executed())
	}
}

func TestInstallNohupFallback(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.systemd = false
	h.installSelf("binary")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser

	report := h.mustInstall(req)

	if report.Mode != ModeNohup || report.ServiceUser != "" {
		t.Fatalf("report = %+v", report)
	}
	if h.run.ran("sudo systemctl") {
		t.Fatalf("systemd was used although it is not running: %v", h.run.executed())
	}
	if len(h.starts) != 1 {
		t.Fatalf("detached starts = %v", h.starts)
	}
	args := h.starts[0]
	joined := strings.Join(args, " ")
	// Both service modes run the server from the configuration alone; nohup puts
	// the supervisor in front of it, standing in for Restart=always.
	if want := h.layout.Binary() + " serve --supervise --config " + h.layout.ConfigPath(); joined != want {
		t.Errorf("detached command line = %q, want %q", joined, want)
	}
	if cfg := h.config(); cfg.Listen.Console[0] != req.Addr || cfg.DataDir != h.layout.DataDir {
		t.Errorf("server.yaml = %+v", cfg)
	}
	if got := readFile(t, h.layout.PIDFile()); strings.TrimSpace(got) != "4242" {
		t.Fatalf("pid file = %q", got)
	}
	if !strings.Contains(h.errOut.String(), "ignoring --service-user "+DefaultServiceUser+" ") {
		t.Fatalf("the operator was not told the service user was dropped: %s", h.errOut.String())
	}
}

// The operator joins the service account's group at install: that membership
// is what makes later upgrades and password resets run without sudo. The hint
// tells them the current shell needs newgrp.
func TestInstallAddsTheOperatorToTheServiceGroup(t *testing.T) {
	t.Setenv("SUDO_USER", "operator")
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser

	report := h.mustInstall(req)
	if !h.run.ran("sudo usermod -aG " + DefaultServiceUser + " operator") {
		t.Fatalf("the operator was not added to the service group: %v", h.run.executed())
	}
	if report.GroupHint == "" || !strings.Contains(report.GroupHint, "newgrp "+DefaultServiceUser) {
		t.Fatalf("group hint = %q", report.GroupHint)
	}
}

func TestInstallSkipsTheGroupWhenAlreadyAMember(t *testing.T) {
	t.Setenv("SUDO_USER", "operator")
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.run.output = map[string]string{"id -nG operator": "operator " + DefaultServiceUser}
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser

	report := h.mustInstall(req)
	if h.run.ran("sudo usermod") {
		t.Fatalf("a member was added again: %v", h.run.executed())
	}
	if report.GroupHint != "" {
		t.Fatalf("group hint = %q", report.GroupHint)
	}
}

// A root operator and a nohup install never touch the group: the former does
// not need it, the latter has no service account.
func TestInstallSkipsTheGroupForRootAndNohup(t *testing.T) {
	t.Setenv("SUDO_USER", "root")
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.mustInstall(h.quiet())
	if h.run.ran("sudo usermod") || h.run.ran("id -nG") {
		t.Fatalf("a root install touched the group: %v", h.run.executed())
	}

	t.Setenv("SUDO_USER", "operator")
	h2 := newHarness(t, t.TempDir())
	h2.installSelf("binary")
	req := h2.quiet()
	req.NoSystemd = true
	h2.mustInstall(req)
	if h2.run.ran("sudo usermod") {
		t.Fatalf("a nohup install touched the group: %v", h2.run.executed())
	}
}

// Even a privileged mkdir cannot repair a regular file where a directory
// belongs. The fixture must not count its recorded sudo call as a repair.
func TestInstallRejectsAFileWhereTheBackupDirectoryBelongs(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	writeFile(t, h.layout.BackupsDir, "not a directory", 0644)
	_, err := h.installer.Install(context.Background(), h.quiet())
	if err == nil || !strings.Contains(err.Error(), "expected a directory") {
		t.Fatalf("blocked backup directory error=%v", err)
	}
	if got := readFile(t, h.layout.BackupsDir); got != "not a directory" {
		t.Fatal("unrelated file was replaced")
	}
	if h.run.ran("sudo systemctl restart gpuworks") {
		t.Fatal("started service with invalid backup directory")
	}
}

// Without a sudo binary a blocked directory fails the install with the
// original error instead of pretending the repair worked.
func TestInstallWithoutSudoFailsOnAPrivilegedDirectory(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.binaries["sudo"] = false
	writeFile(t, h.layout.BackupsDir, "not a directory", 0o644)

	if _, err := h.installer.Install(context.Background(), h.quiet()); err == nil {
		t.Fatal("install succeeded although a directory could not be made")
	}
	if h.run.ran("sudo mkdir") {
		t.Fatalf("a sudo-less install invoked sudo: %v", h.run.executed())
	}
}

// The first install into a fresh /opt happens before the tree is handed to
// the service group, so its writes fall back to sudo; later runs write
// directly. An operator without sudo gets the original error.
func TestInstallFallsBackToSudoForTheFirstTreeWrites(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere, so an unwritable directory cannot be staged")
	}
	h := newHarness(t, t.TempDir())
	self := h.installSelf("binary")
	// A directory the operator cannot write into forces the fallback for the
	// writes that land in it.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(parent, "blocked")
	if err := os.MkdirAll(blocked, 0o555); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.Chmod(blocked, 0o755)
	target := filepath.Join(blocked, "out")
	runner := &privilegedWriteRunner{parent: blocked, target: target}
	h.installer.deps.Run = runner

	ctx := context.Background()
	if err := h.installer.writeInstalled(ctx, target, []byte("content"), 0o644); err != nil {
		t.Fatalf("writeInstalled: %v", err)
	}
	if got := readFile(t, target); got != "content" {
		t.Fatal("privileged write did not publish its content")
	}
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(blocked, "bin")
	if err := h.installer.copyInstalled(ctx, self, bin, 0o755); err != nil {
		t.Fatalf("copyInstalled: %v", err)
	}
	// The running service keeps the target mapped, so the fallback stages a
	// temporary file and renames it — never a direct sudo cp.
	if got := readFile(t, bin); got != "binary" {
		t.Fatal("privileged copy did not publish its content")
	}
	stages := 0
	for _, cmd := range runner.commands {
		if cmd.Path == "mktemp" && cmd.Sudo {
			stages++
		}
		if cmd.Path == "tee" || cmd.Path == "cp" {
			t.Fatal("fallback wrote through a public or predictable target")
		}
	}
	if stages != 2 {
		t.Fatalf("private staging commands=%d, want both write and copy", stages)
	}
	assertNoPrivateStages(t, blocked)
}

func TestInstallTreeWritesFailWithoutSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere, so an unwritable directory cannot be staged")
	}
	h := newHarness(t, t.TempDir())
	h.binaries["sudo"] = false
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.MkdirAll(blocked, 0o555); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.Chmod(blocked, 0o755)

	if err := h.installer.writeInstalled(context.Background(), filepath.Join(blocked, "out"), []byte("x"), 0o644); err == nil {
		t.Fatal("an unwritable tree write succeeded without sudo")
	}
	if h.run.ran("sudo") {
		t.Fatalf("sudo was invoked although it is unavailable: %v", h.run.executed())
	}
}

func TestInstallNoSystemdFlagSkipsTheUnit(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.NoSystemd = true

	if report := h.mustInstall(req); report.Mode != ModeNohup {
		t.Fatalf("mode = %q, want nohup", report.Mode)
	}
	// The harness runs unprivileged, so a unit file could only appear through
	// the recorded `tee`; the real /etc/systemd/system path is the host's and
	// must not be consulted here.
	if _, ok := h.run.writtenFile(h.layout.UnitPath()); ok {
		t.Fatal("--no-systemd must not write a unit file")
	}
}

func TestInstallWithoutPrivilegesExplainsTheUserLevelFallback(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.deps.Euid = func() int { return 1000 }
	h.installer = New(h.layout, h.deps, h.io)
	h.binaries["sudo"] = false
	// A directory only root could create: the mkdir fails, which is how a
	// rootless install notices it has nowhere to write.
	h.layout = NewLayout("/sys/gpuworks-install-test/server", "", "")
	h.installer = New(h.layout, h.deps, h.io)

	_, err := h.installer.Install(context.Background(), h.quiet())
	if err == nil {
		t.Fatal("install succeeded without root and without a writable directory")
	}
	if !strings.Contains(err.Error(), "--dir ~/gpuworks/server") {
		t.Fatalf("the error does not point at the user-level fallback: %v", err)
	}
}

func TestInstallRootlessRunsDetachedInAUserDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gpuworks-server")
	h := newHarness(t, dir)
	h.installSelf("binary")
	h.deps.Euid = func() int { return 1000 }
	h.binaries["sudo"] = false
	h.users[DefaultServiceUser] = true
	h.installer = New(h.layout, h.deps, h.io)

	report := h.mustInstall(h.quiet())
	if report.Mode != ModeNohup {
		t.Fatalf("mode = %q, want nohup", report.Mode)
	}
	if !strings.Contains(h.errOut.String(), "installing without root") {
		t.Fatalf("the warning about the missing supervisor is absent: %s", h.errOut.String())
	}
	if h.run.ran("sudo chown") {
		t.Fatalf("a rootless install cannot chown: %v", h.run.executed())
	}
}

// A sudo binary is all the installer needs to pick systemd: the privileged
// steps are individual sudo commands, and sudo itself asks for the password
// on its first call.
func TestInstallPicksSystemdWhenSudoIsAvailable(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.deps.Euid = func() int { return 1000 }
	h.installer = New(h.layout, h.deps, h.io)

	report := h.mustInstall(h.quiet())
	if report.Mode != ModeSystemd {
		t.Fatalf("mode = %q, want systemd", report.Mode)
	}
	if h.run.ran("sudo -v") {
		t.Fatalf("the install prompted for a sudo password: %v", h.run.executed())
	}
}

// Without a sudo binary there is no way to run the privileged steps, so the
// flow falls back to the rootless nohup path, whether the run is interactive
// or quiet.
func TestInstallWithoutSudoFallsBackToNohup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quiet bool
	}{
		{"interactive", false},
		{"quiet", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.installSelf("binary")
			h.deps.Euid = func() int { return 1000 }
			h.binaries["sudo"] = false
			h.installer = New(h.layout, h.deps, h.io)
			h.terminal = true

			req := h.quiet()
			req.Quiet = tc.quiet
			report := h.mustInstall(req)
			if report.Mode != ModeNohup {
				t.Fatalf("mode = %q, want the rootless nohup fallback", report.Mode)
			}
			if !strings.Contains(h.errOut.String(), "installing without root") {
				t.Fatalf("the rootless warning is missing: %s", h.errOut.String())
			}
		})
	}
}

func TestInstallRefusesToStartWhenThePortIsTaken(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// A previous install of ours is what makes the busy port acceptable: the
	// fake reports the unit as active, so the listener is attributed to it.
	h.writeExistingInstall("v1.0.0")
	req := h.quiet()
	req.Addr = ln.Addr().String()
	req.AddrSet = true

	report := h.mustInstall(req)

	if !strings.Contains(h.out.String(), "held by the running installation") {
		t.Fatalf("the busy port was not explained: %s", h.out.String())
	}
	if report.Mode != ModeSystemd {
		t.Fatalf("mode = %q", report.Mode)
	}
}

func TestInstallRefusesAPortHeldByAnotherProcess(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	h.run.fail["sudo systemctl is-active --quiet gpuworks"] = errors.New("inactive")
	req := h.quiet()
	req.Addr = ln.Addr().String()
	req.AddrSet = true

	_, err = h.installer.Install(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "cannot listen on") {
		t.Fatalf("install did not refuse a port held by another process: %v", err)
	}
	if fileExists(h.layout.Binary()) {
		t.Fatal("nothing should have been written before preflight passed")
	}
}

func TestInstallRefusesASmallDisk(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.freeBytes = 1 << 30

	_, err := h.installer.Install(context.Background(), h.quiet())
	if err == nil || !strings.Contains(err.Error(), "at least 2 GiB") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallRejectsAMalformedAddr(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.Addr = "host:port:extra"

	_, err := h.installer.Install(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "invalid --addr") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallFailsWhenTheServiceNeverAnswers(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.healthErr = errors.New("connection refused")

	_, err := h.installer.Install(context.Background(), h.quiet())
	if err == nil {
		t.Fatal("install succeeded although the server never answered")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.errOut.String(), h.layout.LogFile()) {
		t.Fatalf("the operator was not pointed at the log file: %s", h.errOut.String())
	}
}

func TestInstallKeepsTheExistingDatabaseAndCredentials(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	if err := installcred.WriteInitialCredentials(h.layout.DataDir, DefaultAdminUser, "original"); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	h.installSelf("binary v2")

	req := h.quiet()
	req.Password = "newly-typed"
	report := h.mustInstall(req)

	if report.PasswordSource != PasswordUnchanged || report.Password != "" {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")); got != "old database" {
		t.Fatalf("database = %q", got)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "master.key")); got != "old key" {
		t.Fatalf("master key = %q", got)
	}
	creds, err := installcred.ReadInitialCredentials(h.layout.DataDir)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.Password != "original" {
		t.Fatalf("the credentials file was overwritten: %+v", creds)
	}
	if !strings.Contains(h.errOut.String(), "--password is ignored") {
		t.Fatalf("the ignored flag was not reported: %s", h.errOut.String())
	}
	if !report.Adopted {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "binary v2" {
		t.Fatalf("the binary was not replaced: %q", got)
	}
}

func TestInstallAdoptsTheRecordedLayout(t *testing.T) {
	h := newHarness(t, t.TempDir())
	otherData := filepath.Join(t.TempDir(), "elsewhere")
	h.writeExistingInstall("v1.0.0")
	cfg := h.config()
	cfg.DataDir = otherData
	cfg.Listen.Console = []string{"127.0.0.1:9911"}
	// Keys install does not manage survive a repair.
	cfg.SSHProxy = "socks5://proxy.example:1080"
	cfg.MetricsRetention = "168h"
	h.writeConfig(cfg)
	h.installSelf("binary v2")

	req := h.quiet()
	req.DataDirSet = false
	req.AddrSet = false
	report := h.mustInstall(req)

	if report.Layout.DataDir != otherData {
		t.Fatalf("data dir = %q, want the recorded %q", report.Layout.DataDir, otherData)
	}
	if unit := h.unitFile(); !strings.Contains(unit, "RequiresMountsFor="+otherData) {
		t.Fatalf("the unit does not use the recorded data dir:\n%s", unit)
	}
	after := h.config()
	if after.DataDir != otherData || after.Listen.Console[0] != "127.0.0.1:9911" || after.SSHProxy != "socks5://proxy.example:1080" || after.MetricsRetention != "168h" {
		t.Fatalf("a repair changed server.yaml: %+v", after)
	}
	if report.HealthURL != "http://127.0.0.1:9911/healthz" {
		t.Fatalf("health url = %q", report.HealthURL)
	}
	// A fresh database is created in the adopted directory, so the first-start
	// credentials apply there.
	creds, err := installcred.ReadInitialCredentials(otherData)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.Password != report.Password || creds.Password == "" {
		t.Fatalf("credentials = %+v", creds)
	}

	// An explicit flag still wins over what was recorded.
	h2 := newHarness(t, t.TempDir())
	h2.writeExistingInstall("v1.0.0")
	h2.installSelf("binary v2")
	req2 := h2.quiet()
	req2.Addr = "127.0.0.1:9922"
	req2.AddrSet = true
	if report := h2.mustInstall(req2); report.HealthURL != "http://127.0.0.1:9922/healthz" {
		t.Fatalf("health url = %q", report.HealthURL)
	}
}

func TestInstallStopsTheOtherSupervisionMode(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.supervisedByNohup()
	writeFile(t, h.layout.PIDFile(), "999999\n", 0o644)
	h.installSelf("binary v2")

	report := h.mustInstall(h.quiet())
	if report.Mode != ModeSystemd {
		t.Fatalf("mode = %q", report.Mode)
	}
	if !strings.Contains(h.out.String(), "stopping the previous nohup installation") {
		t.Fatalf("the previous installation was not stopped: %s", h.out.String())
	}
}

func TestInstallQuietGeneratesAPassword(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")

	report := h.mustInstall(h.quiet())
	if report.PasswordSource != PasswordGenerated {
		t.Fatalf("source = %q", report.PasswordSource)
	}
	if len(report.Password) < 8 {
		t.Fatalf("generated password = %q", report.Password)
	}
	creds, err := installcred.ReadInitialCredentials(h.layout.DataDir)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.Password != report.Password {
		t.Fatalf("the printed password is not the one on disk: %+v", creds)
	}
}

// An unattended install takes the admin password from GPUWORKS_ADMIN_PASSWORD
// when --password does not name one; --password still wins.
func TestInstallTakesThePasswordFromTheEnvironment(t *testing.T) {
	t.Setenv("GPUWORKS_ADMIN_PASSWORD", " from-the-environment ")
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.Password = ""

	report := h.mustInstall(req)
	if report.Password != "from-the-environment" || report.PasswordSource != PasswordFromEnv {
		t.Fatalf("report = %+v", report)
	}
	creds, err := installcred.ReadInitialCredentials(h.layout.DataDir)
	if err != nil || creds.Password != "from-the-environment" {
		t.Fatalf("recorded credentials = %+v, %v", creds, err)
	}

	h2 := newHarness(t, t.TempDir())
	h2.installSelf("binary")
	req2 := h2.quiet()
	req2.Password = "from-the-explicit-flag"
	if report := h2.mustInstall(req2); report.Password != "from-the-explicit-flag" || report.PasswordSource != PasswordFromFlag {
		t.Fatalf("--password did not win over the environment: %+v", report)
	}
}

// Interactively the environment password is shown back for confirmation; a
// "no" falls through to the prompt instead of silently using it.
func TestInstallConfirmsTheEnvironmentPasswordInteractively(t *testing.T) {
	t.Setenv("GPUWORKS_ADMIN_PASSWORD", "from-the-environment")
	for _, tc := range []struct {
		name, stdin, want, source string
	}{
		{"accepted", "\n", "from-the-environment", PasswordFromEnv},
		{"declined", "n\nhand-typed-admin-password\n", "hand-typed-admin-password", PasswordFromPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.installSelf("binary")
			h.terminal = true
			h.stdin = tc.stdin
			req := h.quiet()
			req.Password = ""
			req.Quiet = false

			report := h.mustInstall(req)
			if report.Password != tc.want || report.PasswordSource != tc.source {
				t.Fatalf("report = %+v", report)
			}
			if !strings.Contains(h.out.String(), "GPUWORKS_ADMIN_PASSWORD is set to: from-the-environment") {
				t.Fatalf("the environment password was not shown back: %s", h.out.String())
			}
		})
	}
}

func TestInstallAsksForThePasswordInteractively(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.terminal = true
	h.stdin = "hand-typed-admin-password\n"
	req := h.quiet()
	req.Password = ""
	req.Quiet = false

	report := h.mustInstall(req)
	if report.Password != "hand-typed-admin-password" || report.PasswordSource != PasswordFromPrompt {
		t.Fatalf("report = %+v", report)
	}
}

func TestInstallInteractiveBlankPasswordGeneratesOne(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.terminal = true
	h.stdin = "\n"
	req := h.quiet()
	req.Password = ""
	req.Quiet = false

	report := h.mustInstall(req)
	if report.PasswordSource != PasswordGenerated || report.Password == "" {
		t.Fatalf("report = %+v", report)
	}
}

func TestInstallTreatsEndOfInputAsABlankAnswer(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.terminal = true // no scripted input: the operator pressed Ctrl-D
	req := h.quiet()
	req.Password = ""
	req.Quiet = false

	report := h.mustInstall(req)
	if report.PasswordSource != PasswordGenerated || report.Password == "" {
		t.Fatalf("report = %+v", report)
	}
}
