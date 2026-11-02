package provision

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// unitHost is a host whose gpuworks-agent systemd unit was installed for
// unitServer. The probe and restart scripts run for real under sh; systemctl
// is faked with output copied from systemd 243 on a Kylin V10 node.
// unitHostToken is the node token in the unit's ExecStart.
const unitHostToken = "86daa49d7ee62bf7d15face2da996fffcb30da5d84ee6b5e"

type unitHost struct {
	unitServer string // --server in the unit's ExecStart; "" = no unit
	unitToken  string
	unitStart  string
	active     bool
	processes  []string // extra `ps axww` lines, e.g. an agent started by hand

	mu   sync.Mutex
	cmds []string // systemctl invocations, one per line
}

func (h *unitHost) handler(t *testing.T) fakeSSHHandler {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "grep", "sed", "head", "tail", "awk", "test", "cat", "echo"} {
		if real, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	log := filepath.Join(root, "systemctl.log")
	execStart := ""
	if h.unitServer != "" {
		token := h.unitToken
		if token == "" {
			token = unitHostToken
		}
		execStart = "{ path=/opt/gpuworks_other/gpuworks-agent ; argv[]=/opt/gpuworks_other/gpuworks-agent run --server " + h.unitServer +
			" --token " + token + " --connect-mode agent_dial --listen :19090 --state /opt/gpuworks_other ; ignore_errors=no ; start_time=[Tue 2026-10-13 13:11:07 CST] ; stop_time=[n/a] ; pid=2063336 ; code=(null) ; status=0/0 }"
	}
	if h.unitStart != "" {
		execStart = h.unitStart
	}
	active := ""
	if h.active {
		active = "1"
	}
	writeExecutable(t, filepath.Join(bin, "systemctl"), `echo "$*" >> "$FAKE_LOG"
[ -n "$FAKE_EXECSTART" ] || { echo "No files found for gpuworks-agent.service." >&2; exit 1; }
case "$*" in
  "is-active --quiet gpuworks-agent") [ -n "$FAKE_ACTIVE" ] ;;
  "show gpuworks-agent -p ExecStart --value") echo "$FAKE_EXECSTART" ;;
  "cat gpuworks-agent") echo "# /etc/systemd/system/gpuworks-agent.service" ;;
  "restart gpuworks-agent") exit 0 ;;
  *) echo "unexpected systemctl $*" >&2; exit 1 ;;
esac
`)
	// Only the processes the host lists run outside the unit.
	writeExecutable(t, filepath.Join(bin, "ps"), `echo "    PID TTY      STAT   TIME COMMAND"
echo "      1 ?        Ss     0:01 /sbin/init"
[ -n "$FAKE_PS" ] && printf '%s\n' "$FAKE_PS"
exit 0
`)
	// needsSudo asks for the user id; the test host logs in as root.
	writeExecutable(t, filepath.Join(bin, "id"), `echo 0`)
	return func(cmd string, _ io.Reader) (string, string, uint32) {
		c := exec.Command(filepath.Join(bin, "sh"), "-c", cmd)
		c.Env = []string{"PATH=" + bin, "FAKE_LOG=" + log, "FAKE_EXECSTART=" + execStart, "FAKE_ACTIVE=" + active, "FAKE_PS=" + strings.Join(h.processes, "\n")}
		var stdout, stderr strings.Builder
		c.Stdout, c.Stderr = &stdout, &stderr
		status := uint32(0)
		if err := c.Run(); err != nil {
			status = 1
			if exitErr, ok := err.(*exec.ExitError); ok {
				status = uint32(exitErr.ExitCode())
			}
		}
		data, _ := os.ReadFile(log)
		h.mu.Lock()
		h.cmds = strings.Split(strings.TrimSpace(string(data)), "\n")
		h.mu.Unlock()
		return stdout.String(), stderr.String(), status
	}
}

func (h *unitHost) restarted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cmd := range h.cmds {
		if cmd == "restart gpuworks-agent" {
			return true
		}
	}
	return false
}

func probeUnitHost(t *testing.T, h *unitHost, serverURL string) ProbeResult {
	t.Helper()
	host, port := startFakeSSHServer(t, "root", "pw", h.handler(t))
	return (&SSHInstaller{}).Probe(t.Context(), SSHProbeRequest{
		Host: host, Port: port, User: "root", SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
		ServerURL: serverURL, Token: "node-token",
	})
}

func TestProbeIdentifiesTheNodeOnSharedHosts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		host       *unitHost
		serverURL  string
		running    bool
		foreign    string
		wantDetail string
	}{
		{name: "own-unit", host: &unitHost{unitServer: "http://198.51.100.122:8800", unitToken: "node-token", active: true}, serverURL: "http://198.51.100.122:8800/", running: true, wantDetail: "systemd"},
		{name: "own-unit-stopped", host: &unitHost{unitServer: "http://198.51.100.122:8800", unitToken: "node-token"}, serverURL: "http://198.51.100.122:8800", wantDetail: "not-running"},
		{name: "own-unit-wrong-server", host: &unitHost{unitServer: "http://other:8800", unitToken: "node-token", active: true}, serverURL: "http://gw:8800", foreign: "http://other:8800", wantDetail: "foreign-systemd http://other:8800"},
		{name: "another-servers-unit", host: &unitHost{unitServer: "http://192.0.2.156:8800", active: true}, serverURL: "http://198.51.100.122:8800", wantDetail: "not-running"},
		{name: "same-server-other-node", host: &unitHost{unitServer: "http://198.51.100.122:8800", active: true}, serverURL: "http://198.51.100.122:8800", wantDetail: "not-running"},
		{name: "another-servers-unit-stopped", host: &unitHost{unitServer: "http://192.0.2.156:8800"}, serverURL: "http://198.51.100.122:8800", wantDetail: "not-running"},
		{name: "no-unit", host: &unitHost{}, serverURL: "http://198.51.100.122:8800", wantDetail: "not-running"},
		{name: "server-dial-other-unit", host: &unitHost{unitServer: "http://192.0.2.156:8800", active: true}, wantDetail: "not-running"},
		{name: "server-dial-own-unit", host: &unitHost{unitServer: "http://192.0.2.156:8800", unitToken: "node-token", active: true}, running: true, wantDetail: "systemd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeUnitHost(t, tc.host, tc.serverURL)
			if got.Err != nil || !got.Reachable {
				t.Fatalf("probe = %+v", got)
			}
			if got.AgentRunning != tc.running || got.ForeignServerURL != tc.foreign || got.Detail != tc.wantDetail {
				t.Fatalf("probe = %+v, want running=%v foreign=%q detail=%q", got, tc.running, tc.foreign, tc.wantDetail)
			}
			if strings.Contains(got.Detail, "86daa49d") {
				t.Fatalf("probe detail leaked the other server's node token: %q", got.Detail)
			}
		})
	}
}

func TestProbeFindsNohupAgentBesideOtherUnits(t *testing.T) {
	for _, unitServer := range []string{"", "http://gw:8800", "http://other:8800"} {
		for _, active := range []bool{false, true} {
			t.Run(fmt.Sprintf("unit=%s/active=%v", unitServer, active), func(t *testing.T) {
				h := &unitHost{unitServer: unitServer, active: active, processes: []string{
					"/opt/gpuworks/gpuworks-agent run --state /opt/gpuworks --server http://gw:8800 --token node-token",
				}}
				got := probeUnitHost(t, h, "http://gw:8800")
				if got.Err != nil || !got.AgentRunning || got.ForeignServerURL != "" || got.Detail != "process" {
					t.Fatalf("running target: %+v", got)
				}
				h.processes = nil
				got = probeUnitHost(t, h, "http://gw:8800")
				if got.Err != nil || got.AgentRunning || got.ForeignServerURL != "" || got.Detail != "not-running" {
					t.Fatalf("stopped target: %+v", got)
				}
				if h.restarted() {
					t.Fatal("probing restarted the shared unit")
				}
			})
		}
	}
}

func TestRestartAgentDoesNotRestartAnotherServersUnit(t *testing.T) {
	foreign := &unitHost{unitServer: "http://192.0.2.156:8800"}
	host, port := startFakeSSHServer(t, "root", "pw", foreign.handler(t))
	req := SSHProbeRequest{
		Host: host, Port: port, User: "root", SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
		ServerURL: "http://198.51.100.122:8800", Token: "node-token",
	}
	// The unit is another agent's: the restart falls through to this node's
	// own nohup agent, which this host does not have.
	err := (&SSHInstaller{}).RestartAgent(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "agent binary is not installed") {
		t.Fatalf("RestartAgent err = %v", err)
	}
	if foreign.restarted() {
		t.Fatalf("restarted another server's unit: %v", foreign.cmds)
	}

	// A unit for this server but another node (a host running several
	// agents) is not this node's either.
	sibling := &unitHost{unitServer: "http://198.51.100.122:8800"}
	host, port = startFakeSSHServer(t, "root", "pw", sibling.handler(t))
	req.Host, req.Port = host, port
	if err := (&SSHInstaller{}).RestartAgent(t.Context(), req); err == nil || sibling.restarted() {
		t.Fatalf("restarted another node's unit: err=%v %v", err, sibling.cmds)
	}

	own := &unitHost{unitServer: "http://198.51.100.122:8800"}
	host, port = startFakeSSHServer(t, "root", "pw", own.handler(t))
	req.Host, req.Port = host, port
	req.Token = unitHostToken
	if err := (&SSHInstaller{}).RestartAgent(t.Context(), req); err != nil {
		t.Fatalf("RestartAgent own unit: %v", err)
	}
	if !own.restarted() {
		t.Fatalf("own unit was not restarted: %v", own.cmds)
	}
}

func TestRestartAgentFindsConfiguredUnitInSpacedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gpu works")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gpuworks-agent.conf"), []byte("SERVER=http://gw\nTOKEN=node-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &unitHost{unitStart: "{ argv[]=" + dir + "/gpuworks-agent run --state " + dir + " ; ignore_errors=no ; }"}
	host, port := startFakeSSHServer(t, "root", "pw", h.handler(t))
	err := (&SSHInstaller{}).RestartAgent(t.Context(), SSHProbeRequest{
		Host: host, Port: port, User: "root", SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
		ServerURL: "http://gw", Token: "node-token", InstallDir: dir,
	})
	if err != nil || !h.restarted() {
		t.Fatalf("restart=%v, restarted own unit=%v", err, h.restarted())
	}
}
