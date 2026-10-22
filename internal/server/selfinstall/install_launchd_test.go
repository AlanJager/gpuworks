package selfinstall

import (
	"errors"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/launchd"
)

// macHarness is a Mac: no systemd, launchd as init, and an operator (alice)
// installing with sudo. launchctl answers the way macOS does: print fails for
// a label launchd does not hold, bootstrap loads and starts it, bootout lets
// it go.
func macHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("SUDO_USER", "alice")
	h := newHarness(t, t.TempDir())
	h.systemd, h.launchd = false, true
	h.binaries = map[string]bool{"sudo": true, "launchctl": true, "ps": true}
	h.run.output["id -gn alice"] = "staff"
	service := "sudo launchctl print " + launchdDomain + "/" + launchd.ServerLabel
	h.run.fail[service] = errors.New("exit status 113")
	h.run.on = func(cmd Command) {
		text := cmd.String()
		h.run.mu.Lock()
		defer h.run.mu.Unlock()
		switch {
		case strings.HasPrefix(text, "sudo launchctl bootstrap "+launchdDomain+" "):
			delete(h.run.fail, service)
			h.run.output[service] = "system/ai.gpuworks.server = {\n\tstate = running\n\tpid = 7001\n}"
		case text == "sudo launchctl bootout "+launchdDomain+"/"+launchd.ServerLabel:
			h.run.fail[service] = errors.New("exit status 113")
		}
	}
	return h
}

func TestInstallOnAMacRegistersALaunchDaemonRunningAsTheInstaller(t *testing.T) {
	h := macHarness(t)
	h.installSelf("gpuworks binary v1")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	req.Password = "s3cret"

	report := h.mustInstall(req)

	if report.Mode != ModeLaunchd || report.ServiceUser != "alice" {
		t.Fatalf("report = %+v (stderr: %s)", report, h.errOut.String())
	}
	plist, ok := h.run.writtenFile(h.layout.LaunchdPath())
	if !ok {
		t.Fatalf("no plist written: %v", h.run.executed())
	}
	spec, err := launchd.ReadSpec([]byte(plist))
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{h.layout.Binary(), "serve", "--config", h.layout.ConfigPath()}
	if spec.Label != launchd.ServerLabel || spec.User != "alice" || strings.Join(spec.Args, " ") != strings.Join(wantArgs, " ") ||
		spec.Env[LaunchdEnv] != "1" || spec.StderrPath != h.layout.StderrLog() || spec.WorkingDir != h.layout.Dir {
		t.Fatalf("plist = %+v\n%s", spec, plist)
	}
	for _, want := range []string{"<key>KeepAlive</key><true/>", "<key>ExitTimeOut</key><integer>30</integer>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %s:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "AbandonProcessGroup") {
		t.Error("the server's children must stop with it")
	}
	// The plist is world-readable: no secret, and no setting server.yaml holds.
	if strings.Contains(plist, req.Password) || strings.Contains(plist, "--addr") {
		t.Fatalf("the plist carries a secret or a setting:\n%s", plist)
	}
	if !h.run.ranInOrder("sudo tee "+h.layout.LaunchdPath(), "sudo chown root:wheel "+h.layout.LaunchdPath(),
		"sudo launchctl bootstrap "+launchdDomain+" "+h.layout.LaunchdPath()) {
		t.Fatalf("launchd was not driven in order: %v", h.run.executed())
	}
	// The tree belongs to the account that runs the server, in its own group.
	if !h.run.ran("sudo chown -R alice:staff " + h.layout.Dir) {
		t.Fatalf("the tree was not given to alice: %v", h.run.executed())
	}
	// A Mac has no service account to create or join.
	for _, linuxOnly := range []string{"sudo useradd", "sudo usermod", "sudo systemctl"} {
		if h.run.ran(linuxOnly) {
			t.Fatalf("ran %s on a Mac: %v", linuxOnly, h.run.executed())
		}
	}
	// The default --service-user is not something the operator asked for.
	if strings.Contains(h.errOut.String(), "service-user") || strings.Contains(h.errOut.String(), "systemd") {
		t.Fatalf("warned about systemd or the service user: %s", h.errOut.String())
	}
}

func TestALaunchDaemonInstallationRestartsAndStopsThroughLaunchd(t *testing.T) {
	h := macHarness(t)
	h.installSelf("gpuworks binary v1")
	h.mustInstall(h.quiet())

	// A later command reads the mode from the plist, as it reads the unit.
	inst, err := h.installer.readInstallation(t.Context())
	if err != nil || inst.Mode != ModeLaunchd || inst.ServiceUser != "alice" {
		t.Fatalf("installation = %+v, %v", inst, err)
	}

	h.run.calls = nil
	if mode, err := New(h.layout, h.deps, h.io).Restart(t.Context()); err != nil || mode != ModeLaunchd {
		t.Fatalf("Restart = %q, %v", mode, err)
	}
	service := launchdDomain + "/" + launchd.ServerLabel
	if !h.run.ranInOrder("sudo launchctl bootout "+service, "sudo launchctl bootstrap "+launchdDomain+" "+h.layout.LaunchdPath()) {
		t.Fatalf("restart did not reload the daemon: %v", h.run.executed())
	}

	h.run.calls = nil
	if mode, err := New(h.layout, h.deps, h.io).Stop(t.Context()); err != nil || mode != ModeLaunchd {
		t.Fatalf("Stop = %q, %v", mode, err)
	}
	if !h.run.ran("sudo launchctl bootout "+service) || h.run.ran("sudo rm") {
		t.Fatalf("stop: %v", h.run.executed())
	}
	// Stopped, it stays installed: the plist brings it back at boot.
	if _, err := os.Stat(h.layout.LaunchdPath()); err != nil {
		t.Fatalf("stop removed the plist: %v", err)
	}
	if New(h.layout, h.deps, h.io).serviceActive(t.Context()) {
		t.Fatal("a stopped daemon reads as active")
	}
}

func TestALaunchDaemonRunsAsTheInstallerWhateverServiceUserSays(t *testing.T) {
	for _, tc := range []struct {
		name, operator, flag string
		set                  bool
		wantUser, wantWarn   string
	}{
		{"default flag", "alice", DefaultServiceUser, false, "alice", ""},
		{"another account named", "alice", "bob", true, "alice", "runs as the account that installs it (alice); ignoring --service-user bob"},
		{"root installs", "root", DefaultServiceUser, false, "", "the launch daemon runs as root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, warnings := launchdUser(InstallRequest{ServiceUser: tc.flag, ServiceUserSet: tc.set}, tc.operator, nil)
			joined := strings.Join(warnings, "\n")
			if user != tc.wantUser || (tc.wantWarn == "") != (joined == "") || !strings.Contains(joined, tc.wantWarn) {
				t.Fatalf("launchdUser = %q, %q", user, joined)
			}
		})
	}
}

// Moving a Mac installation to the detached mode removes the plist: its
// presence is what marks the launchd mode, and launchd would start the server
// again at boot beside the detached one.
func TestMovingOffLaunchdRemovesTheDaemon(t *testing.T) {
	h := macHarness(t)
	h.installSelf("gpuworks binary v1")
	h.mustInstall(h.quiet())

	req := h.quiet()
	req.NoSystemd = true
	report := h.mustInstall(req)
	if report.Mode != ModeNohup {
		t.Fatalf("mode = %q", report.Mode)
	}
	if !h.run.ranInOrder("sudo launchctl bootout "+launchdDomain+"/"+launchd.ServerLabel, "sudo rm -f "+h.layout.LaunchdPath()) {
		t.Fatalf("the daemon was not removed: %v", h.run.executed())
	}
	if inst, err := h.installer.readInstallation(t.Context()); err != nil || inst.Mode != ModeNohup {
		t.Fatalf("installation = %+v, %v", inst, err)
	}
}

func TestManagedModeNamesWhatRestartsTheServer(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv(LaunchdEnv, "")
	if got := ManagedMode(); got != "" {
		t.Fatalf("unmanaged = %q", got)
	}
	t.Setenv(LaunchdEnv, "1")
	if got := ManagedMode(); got != ModeLaunchd {
		t.Fatalf("under launchd = %q", got)
	}
	t.Setenv("INVOCATION_ID", "abc")
	if got := ManagedMode(); got != ModeSystemd {
		t.Fatalf("under systemd = %q", got)
	}
}
