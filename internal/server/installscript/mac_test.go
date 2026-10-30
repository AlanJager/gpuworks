package installscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// macHost runs the install script as if on a Mac: uname, id, sw_vers, curl,
// chown and (optionally) sudo are stubs that record what they were asked, the
// agent binary the script installs records how it was run, and every other
// command the script needs is the real one. PATH holds only these, so a real
// sudo on the test machine is never reached. What the agent then does with
// launchd is package launchd's, tested there.
type macHost struct {
	t       *testing.T
	dir     string // stub bin
	log     string // calls of the recording stubs
	home    string
	version string // what sw_vers prints
}

// agentStub is the binary the script installs: it records its arguments and
// the token it reads from stdin, and fails when FAIL_SERVICE is set.
const agentStub = `#!/bin/sh
read -r token
echo "agent $* token=$token" >> "$CALLS"
if [ -n "${FAIL_SERVICE:-}" ]; then echo "gpuworks: launchd refused the job" >&2; exit 1; fi
`

func newMacHost(t *testing.T, arch, uid, user string, withSudo bool) *macHost {
	t.Helper()
	h := &macHost{t: t, dir: t.TempDir(), home: t.TempDir(), version: "15.4.1"}
	h.log = filepath.Join(t.TempDir(), "calls")
	for _, tool := range []string{"sh", "cat", "cp", "sed", "mkdir", "rm", "chmod", "mv", "printf", "echo", "tail"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(real, filepath.Join(h.dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(h.dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	record := `echo "${0##*/} $*" >> "$CALLS"`
	stub("uname", `case "$1" in -m) echo `+arch+` ;; -s) echo Darwin ;; esac`)
	stub("id", `case "$1" in -u) echo `+uid+` ;; -g) echo 20 ;; -un) echo `+user+` ;; esac`)
	stub("sw_vers", `echo "$MACOS_VERSION"`)
	stub("curl", record+`
while [ $# -gt 0 ]; do [ "$1" = "-o" ] && { cat > "$2" <<'AGENT'
`+agentStub+`AGENT
}; shift; done`)
	stub("chown", record)
	if withSudo {
		stub("sudo", record+`
[ "$1" = "-n" ] && shift
"$@"`)
	}
	return h
}

func (h *macHost) run(env map[string]string, args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.dir, "sh"), append([]string{"-c", Script, "install.sh"}, args...)...)
	// The host's own /opt/gpuworks must not decide where the test installs.
	cmd.Env = []string{"PATH=" + h.dir, "HOME=" + h.home, "CALLS=" + h.log, "MACOS_VERSION=" + h.version,
		"GPUWORKS_MAC_SYSTEM_DIR=" + filepath.Join(h.home, "no-opt")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *macHost) calls() string {
	data, _ := os.ReadFile(h.log)
	return string(data)
}

func TestScriptOnAMacWithoutAdministratorRightsRegistersALaunchAgent(t *testing.T) {
	h := newMacHost(t, "arm64", "501", "alice", false)
	out, err := h.run(nil, "--server", "http://gw:8800", "--token", "tok&en")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	installDir := filepath.Join(h.home, "gpuworks")
	calls := h.calls()
	for _, want := range []string{
		"curl -fsSL http://gw:8800/dl/gpuworks-agent-darwin-arm64",
		"agent configure --state " + installDir + " --connect-mode agent_dial --listen  --driver  --mount-root  --udocker-bundle  --server http://gw:8800 token=tok&en",
		"agent install-service --state " + installDir + " token=\n",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "--user") || strings.Contains(calls, "sudo") {
		t.Fatalf("a launch agent install asked for a user or sudo:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(installDir, "gpuworks-agent")); err != nil {
		t.Fatalf("agent binary not installed: %v", err)
	}
	if !strings.Contains(out, "gpuworks: installed in "+installDir+"\n") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestScriptOnAMacAsAdministratorRegistersADaemonForTheInstallingUser(t *testing.T) {
	// Run under sudo: root, with the account sudo was run from.
	h := newMacHost(t, "arm64", "0", "root", false)
	installDir := filepath.Join(t.TempDir(), "opt")
	out, err := h.run(map[string]string{"SUDO_USER": "alice", "GPUWORKS_INSTALL_DIR": installDir}, "--server", "http://gw:8800", "--token", "t")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(h.calls(), "agent configure --state "+installDir+" --connect-mode agent_dial") || !strings.Contains(h.calls(), "token=t\n") ||
		!strings.Contains(h.calls(), "agent install-service --state "+installDir+" --user alice token=\n") {
		t.Fatalf("calls:\n%s", h.calls())
	}

	// With passwordless sudo the binary registers the daemon through it.
	h = newMacHost(t, "arm64", "501", "alice", true)
	out, err = h.run(map[string]string{"GPUWORKS_INSTALL_DIR": installDir}, "--server", "http://gw:8800", "--token", "t")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(h.calls(), "sudo -n "+installDir+"/gpuworks-agent install-service --state "+installDir+" --user alice") {
		t.Fatalf("calls:\n%s", h.calls())
	}
}

// The SSH onboarding uploads the binary and runs the script under sudo with
// GPUWORKS_AGENT_BINARY: nothing is downloaded, so a Mac that cannot reach the
// server (server_dial) installs the same way.
func TestScriptInstallsAnUploadedBinaryWithoutDownloading(t *testing.T) {
	h := newMacHost(t, "arm64", "0", "root", false)
	uploaded := filepath.Join(t.TempDir(), "gpuworks-agent.123")
	if err := os.WriteFile(uploaded, []byte(agentStub), 0o755); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "Desktop", "test")
	out, err := h.run(map[string]string{"SUDO_USER": "xixi", "GPUWORKS_AGENT_BINARY": uploaded},
		"--server", "http://gw:8800", "--token", "t", "--install-dir", installDir)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if strings.Contains(h.calls(), "curl") {
		t.Fatalf("downloaded although a binary was uploaded:\n%s", h.calls())
	}
	if !strings.Contains(h.calls(), "--state "+installDir+" ") || !strings.Contains(h.calls(), "--user xixi") {
		t.Fatalf("calls:\n%s", h.calls())
	}
	if _, err := os.Stat(uploaded); err != nil {
		t.Fatalf("the uploaded file belongs to the caller, which removes its staging directory: %v", err)
	}
}

func TestScriptFailsWhenTheAgentCannotRegisterItsJob(t *testing.T) {
	h := newMacHost(t, "arm64", "501", "alice", false)
	out, err := h.run(map[string]string{"FAIL_SERVICE": "1"}, "--server", "http://gw:8800", "--token", "t")
	if err == nil || !strings.Contains(out, "launchd refused") || strings.Contains(out, "installed in") {
		t.Fatalf("install = %v\n%s", err, out)
	}
}

func TestScriptRefusesAMacItCannotServe(t *testing.T) {
	for name, h := range map[string]*macHost{
		"intel":     newMacHost(t, "x86_64", "501", "alice", false),
		"macOS 14":  newMacHost(t, "arm64", "501", "alice", false),
		"no answer": newMacHost(t, "arm64", "501", "alice", false),
	} {
		want := "macOS 15 or later"
		switch name {
		case "intel":
			want = "Apple silicon"
		case "macOS 14":
			h.version = "14.7.2"
		case "no answer":
			h.version = ""
		}
		out, err := h.run(nil, "--server", "http://gw:8800", "--token", "t")
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !strings.Contains(out, want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
		if strings.Contains(h.calls(), "curl") {
			t.Errorf("%s: downloaded an agent", name)
		}
	}
}

// With administrator rights the agent still lives in the installing user's
// home on a Mac; only the launch daemon needs sudo.
func TestScriptOnAMacAsAdministratorDefaultsToTheHome(t *testing.T) {
	h := newMacHost(t, "arm64", "501", "alice", true)
	out, err := h.run(nil, "--server", "http://gw:8800", "--token", "t")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	installDir := filepath.Join(h.home, "gpuworks")
	if !strings.Contains(h.calls(), "sudo -n "+installDir+"/gpuworks-agent install-service --state "+installDir+" --user alice") {
		t.Fatalf("calls:\n%s", h.calls())
	}
	if !strings.Contains(out, "gpuworks: installed in "+installDir+"\n") {
		t.Fatalf("output:\n%s", out)
	}
}

// An agent an earlier install left in /opt/gpuworks or ~/.gpuworks is
// reinstalled where it is, with its models, rather than beside a fresh one.
func TestScriptOnAMacKeepsAnEarlierInstallDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(h *macHost) string
	}{
		{"system", func(h *macHost) string { return filepath.Join(h.home, "no-opt") }},
		{"legacy home", func(h *macHost) string { return filepath.Join(h.home, ".gpuworks") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMacHost(t, "arm64", "501", "alice", true)
			dir := tc.dir(h)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "gpuworks-agent.conf"), []byte("TOKEN=old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := h.run(nil, "--server", "http://gw:8800", "--token", "t")
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if !strings.Contains(h.calls(), "install-service --state "+dir+" ") {
				t.Fatalf("calls:\n%s", h.calls())
			}
			if _, err := os.Stat(filepath.Join(h.home, "gpuworks")); !os.IsNotExist(err) {
				t.Fatalf("a second agent directory was created: %v", err)
			}
		})
	}
}

// A quoted ~ reaches the script unexpanded; it must land in the home, not in
// a directory named "~".
func TestScriptExpandsAQuotedHome(t *testing.T) {
	h := newMacHost(t, "arm64", "501", "alice", false)
	out, err := h.run(nil, "--server", "http://gw:8800", "--token", "t", "--install-dir", "~/gw-agent")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	installDir := filepath.Join(h.home, "gw-agent")
	if !strings.Contains(h.calls(), "install-service --state "+installDir+" ") {
		t.Fatalf("calls:\n%s", h.calls())
	}
	if _, err := os.Stat(filepath.Join(installDir, "gpuworks-agent")); err != nil {
		t.Fatalf("agent binary not installed: %v", err)
	}
}
