package installscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// linuxAgentStub is the binary the script installs on a Linux host: it
// records how it was run and, for configure, writes the server and the token
// it reads from stdin where the real agent writes its configuration.
const linuxAgentStub = `#!/bin/sh
echo "agent $*" >> "$CALLS"
if [ "$1" = configure ]; then
  shift
  state=""; server=""
  while [ $# -gt 0 ]; do
    case "$1" in --state) state="$2"; shift 2 ;; --server) server="$2"; shift 2 ;; *) shift ;; esac
  done
  read -r token
  printf 'SERVER=%s\nTOKEN=%s\n' "$server" "$token" > "$state/gpuworks-agent.conf"
fi
exit 0
`

// linuxHost runs the install script as root on a Linux host whose systemd
// is faked: systemctl answers from a unit file the test writes (its ExecStart
// as systemd 243 prints it) and records what it is asked, tee captures the
// unit the script writes. Everything else is the real tool.
type linuxHost struct {
	t        *testing.T
	bin      string
	calls    string
	unit     string // ExecStart of the installed unit; absent = no unit
	written  string // the unit the script wrote
	stateDir string
	uploaded string
	realRM   string
}

func newLinuxHost(t *testing.T) *linuxHost {
	t.Helper()
	h := &linuxHost{t: t, bin: t.TempDir(), stateDir: filepath.Join(t.TempDir(), "gpuworks")}
	work := t.TempDir()
	h.calls, h.unit, h.written = filepath.Join(work, "calls"), filepath.Join(work, "unit"), filepath.Join(work, "written")
	for _, tool := range []string{"sh", "cat", "cp", "sed", "mkdir", "chmod", "mv", "printf", "echo", "nohup", "tr", "awk", "tail", "head"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(real, filepath.Join(h.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	realRM, err := exec.LookPath("rm")
	if err != nil {
		t.Skip("no rm")
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(h.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("uname", `case "$1" in -m) echo x86_64 ;; -s) echo Linux ;; esac`)
	stub("id", `case "$1" in -u|-g) echo 0 ;; esac`)
	stub("curl", `echo "curl $*" >> "$CALLS"; exit 22`)
	stub("systemctl", `echo "systemctl $*" >> "$CALLS"
case "$1" in
  cat) [ -e "$UNIT" ] ;;
  show) [ -e "$UNIT" ] && cat "$UNIT" ;;
esac
exit 0`)
	stub("tee", `echo "tee $*" >> "$CALLS"; cat > "$WRITTEN"`)
	stub("rm", `echo "rm $*" >> "$CALLS"
for f; do case "$f" in /etc/*|-*) ;; *) "$REAL_RM" -f "$f" ;; esac; done`)
	h.uploaded = filepath.Join(t.TempDir(), "gpuworks-agent.upload")
	if err := os.WriteFile(h.uploaded, []byte(linuxAgentStub), 0o755); err != nil {
		t.Fatal(err)
	}
	h.realRM = realRM
	return h
}

// withUnit installs a gpuworks-agent unit whose agent runs with execStart's
// arguments.
func (h *linuxHost) withUnit(args string) {
	h.t.Helper()
	line := "{ path=/opt/gpuworks/gpuworks-agent ; argv[]=/opt/gpuworks/gpuworks-agent run " + args + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
	if err := os.WriteFile(h.unit, []byte(line+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *linuxHost) run(extraEnv []string, args ...string) (string, error) {
	h.t.Helper()
	full := append([]string{"-c", Script, "install.sh", "--install-dir", h.stateDir}, args...)
	cmd := exec.Command(filepath.Join(h.bin, "sh"), full...)
	cmd.Env = []string{"PATH=" + h.bin, "HOME=" + h.t.TempDir(), "CALLS=" + h.calls, "UNIT=" + h.unit, "WRITTEN=" + h.written, "GPUWORKS_AGENT_BINARY=" + h.uploaded, "REAL_RM=" + h.realRM}
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *linuxHost) read(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func requireSystemd(t *testing.T) {
	t.Helper()
	if fi, err := os.Stat("/run/systemd/system"); err != nil || !fi.IsDir() {
		t.Skip("the systemd branch requires a host running systemd")
	}
}

// A node the server dials needs no server address: the uploaded binary is
// installed, the configuration names no server, and nothing is downloaded.
func TestScriptServerDialInstallsWithoutAServer(t *testing.T) {
	h := newLinuxHost(t)
	out, err := h.run([]string{"GPUWORKS_NO_SYSTEMD=1"}, "--token", "node-token", "--connect-mode", "server_dial", "--listen", "0.0.0.0:6006")
	if err != nil {
		t.Fatalf("install = %v\n%s", err, out)
	}
	calls := h.read(h.calls)
	if strings.Contains(calls, "curl") || strings.Contains(calls, "--server") {
		t.Fatalf("a server_dial install used a server:\n%s", calls)
	}
	if !strings.Contains(calls, "agent configure --state "+h.stateDir+" --connect-mode server_dial --listen 0.0.0.0:6006 ") {
		t.Fatalf("configure was not run with the listen address:\n%s", calls)
	}
	if conf := h.read(filepath.Join(h.stateDir, "gpuworks-agent.conf")); conf != "SERVER=\nTOKEN=node-token\n" {
		t.Fatalf("configuration = %q", conf)
	}
	restart := h.read(filepath.Join(h.stateDir, "agent-restart.sh"))
	if !strings.Contains(restart, `nohup "`+h.stateDir+`/gpuworks-agent" run --state "`+h.stateDir+`"`) || strings.Contains(restart, "node-token") {
		t.Fatalf("restart script:\n%s", restart)
	}
	if !strings.Contains(out, "started via nohup") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestScriptRefusesWhatItCannotInstall(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{"agent dial without server", nil, []string{"--token", "t"}, "--server is required when the agent connects"},
		{"no token", nil, []string{"--server", "http://gw:8800"}, "--token is required"},
		{"no binary", []string{"GPUWORKS_AGENT_BINARY="}, []string{"--token", "t", "--connect-mode", "server_dial"}, "no agent binary"},
		{"mode", nil, []string{"--token", "t", "--connect-mode", "both"}, "--connect-mode must be"},
		{"supervisor", nil, []string{"--server", "http://gw:8800", "--token", "t", "--supervisor", "cron"}, "--supervisor must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinuxHost(t)
			out, err := h.run(tc.env, tc.args...)
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(out, tc.want) {
				t.Fatalf("install = %v\n%s", err, out)
			}
			if _, err := os.Stat(h.stateDir); !os.IsNotExist(err) {
				t.Fatalf("a refused install changed the host: %v", err)
			}
		})
	}
}

// The unit runs `run --state DIR` and holds no configuration: the token is
// in the 0600 file, not in ExecStart where ps and systemctl show it.
func TestScriptWritesAUnitThatRunsTheStateDirectory(t *testing.T) {
	requireSystemd(t)
	h := newLinuxHost(t)
	out, err := h.run(nil, "--server", "http://gw:8800", "--token", "node-token", "--driver", "udocker")
	if err != nil {
		t.Fatalf("install = %v\n%s", err, out)
	}
	unit := h.read(h.written)
	for _, want := range []string{
		"RequiresMountsFor=" + h.stateDir + " " + h.stateDir + "\n",
		"ExecStartPre=/usr/bin/test -x " + h.stateDir + "/gpuworks-agent\n",
		"ExecStart=" + h.stateDir + "/gpuworks-agent run --state " + h.stateDir + "\n",
		"KillMode=process\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	for _, refused := range []string{"node-token", "--server", "Environment="} {
		if strings.Contains(unit, refused) {
			t.Fatalf("unit carries %q:\n%s", refused, unit)
		}
	}
	calls := h.read(h.calls)
	for _, want := range []string{"agent configure --state " + h.stateDir + " --connect-mode agent_dial --listen  --driver udocker", "systemctl enable gpuworks-agent", "systemctl restart gpuworks-agent"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls lack %q:\n%s", want, calls)
		}
	}
	if !strings.Contains(out, "started via systemd") {
		t.Fatalf("output:\n%s", out)
	}
}

// Keep (restart and recovery) takes the unit only when it runs this node's
// agent: by the token in the state directory's configuration, or for an
// agent installed before the file, by the token in its arguments. A unit for
// another node is left alone and this node's agent runs under nohup.
func TestScriptKeepTakesOnlyThisNodesUnit(t *testing.T) {
	requireSystemd(t)
	for _, tc := range []struct {
		name, owner string
		systemd     bool
	}{
		{"configured for this node", "conf:node-token", true},
		{"configured for another node", "conf:other-token", false},
		{"arguments of this node", "args:node-token", true},
		{"arguments of another node", "args:other-token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinuxHost(t)
			kind, token, _ := strings.Cut(tc.owner, ":")
			unitState := t.TempDir()
			if kind == "conf" {
				if err := os.WriteFile(filepath.Join(unitState, "gpuworks-agent.conf"), []byte("SERVER=http://gw:8800\nTOKEN="+token+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				h.withUnit("--state " + unitState)
			} else {
				h.withUnit("--server http://gw:8800 --token " + token + " --connect-mode agent_dial --listen :19090 --state " + unitState)
			}
			out, err := h.run(nil, "--server", "http://gw:8800", "--token", "node-token", "--supervisor", "keep")
			if err != nil {
				t.Fatalf("install = %v\n%s", err, out)
			}
			wroteUnit := strings.Contains(h.read(h.calls), "tee /etc/systemd/system/gpuworks-agent.service")
			if wroteUnit != tc.systemd || strings.Contains(out, "started via systemd") != tc.systemd {
				t.Fatalf("systemd = %v, want %v\n%s\n%s", wroteUnit, tc.systemd, out, h.read(h.calls))
			}
		})
	}
}

// A nohup install stays on nohup through keep even where systemd runs and
// the user is root: agent-restart.sh without a unit is the user's choice.
func TestScriptKeepStaysOnNohupForANohupInstall(t *testing.T) {
	h := newLinuxHost(t)
	h.stateDir = filepath.Join(t.TempDir(), "gpu works")
	if out, err := h.run(nil, "--server", "http://gw:8800", "--token", "t", "--supervisor", "nohup"); err != nil {
		t.Fatalf("install = %v\n%s", err, out)
	}
	_ = os.Remove(h.calls)
	out, err := h.run(nil, "--server", "http://gw:8800", "--token", "t", "--supervisor", "keep")
	if err != nil || !strings.Contains(out, "started via nohup") || strings.Contains(h.read(h.calls), "tee") {
		t.Fatalf("keep moved a nohup install off nohup: %v\n%s\n%s", err, out, h.read(h.calls))
	}
}

// Replacing the agent another server installed says so, whether the unit or
// the state directory's configuration names that server.
func TestScriptWarnsBeforeReplacingAnotherServersAgent(t *testing.T) {
	for _, tc := range []struct {
		name, server, want string
		unit               bool
	}{
		{name: "configuration of another server", server: "http://172.25.17.156:8800", want: "replacing the gpuworks agent installed for another server (http://172.25.17.156:8800)"},
		{name: "configuration of this server", server: "http://gw:8800/"},
		{name: "unit of another server", server: "http://172.25.17.156:8800", unit: true, want: "another server (http://172.25.17.156:8800)"},
		{name: "nothing installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unit {
				requireSystemd(t)
			}
			h := newLinuxHost(t)
			if tc.unit {
				h.withUnit("--server " + tc.server + " --token x --state /nonexistent")
			} else if tc.server != "" {
				if err := os.MkdirAll(h.stateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h.stateDir, "gpuworks-agent.conf"), []byte("SERVER="+tc.server+"\nTOKEN=x\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{"GPUWORKS_NO_SYSTEMD=1"}
			if tc.unit {
				env = nil
			}
			out, err := h.run(env, "--server", "http://gw:8800", "--token", "t")
			if err != nil {
				t.Fatalf("install = %v\n%s", err, out)
			}
			if warned := strings.Contains(out, "WARNING"); warned != (tc.want != "") || !strings.Contains(out, tc.want) {
				t.Fatalf("output = %q, want warning %q", out, tc.want)
			}
		})
	}
}
