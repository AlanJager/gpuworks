package provision

import (
	"os/exec"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

func TestInstallSupervisorFollowsTheRequest(t *testing.T) {
	cases := []struct {
		req  SSHInstallRequest
		want agentSupervisor
	}{
		{SSHInstallRequest{}, supervisorSystemd},
		{SSHInstallRequest{NoSystemd: true}, supervisorNohup},
		{SSHInstallRequest{NoSystemd: true, KeepSupervisor: true}, supervisorKeep},
	}
	for _, tc := range cases {
		if got := tc.req.supervisor(); got != tc.want {
			t.Fatalf("supervisor(%+v) = %d, want %d", tc.req, got, tc.want)
		}
	}
}

// The SSH install hands install.sh exactly the configuration the request
// describes: no server for a node the server dials, its listen address, and
// the flags the install chose.
func TestScriptInstallArgsCarryTheRequestsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  SSHInstallRequest
		s    scriptInstall
		want string
	}{
		{
			name: "agent dial",
			req:  SSHInstallRequest{ServerURL: "https://gw.example/", Token: "tok'en", RuntimeDriver: "udocker", UdockerBundle: "/opt/gpuworks/offline/udocker"},
			s:    scriptInstall{installDir: "/opt/gpu works", supervisor: supervisorKeep},
			want: "--install-dir '/opt/gpu works' --token 'tok'\\''en' --connect-mode 'agent_dial' --supervisor 'keep' --server 'https://gw.example' --driver 'udocker' --udocker-bundle '/opt/gpuworks/offline/udocker'",
		},
		{
			name: "server dial",
			req:  SSHInstallRequest{ServerURL: "", Token: "t", ConnectMode: "server_dial", ServerDialListen: "6006"},
			s:    scriptInstall{installDir: "/tmp/gpuworks", supervisor: supervisorNohup, noSudo: true},
			want: "--install-dir '/tmp/gpuworks' --token 't' --connect-mode 'server_dial' --supervisor 'nohup' --listen ':6006' --no-sudo",
		},
		{
			name: "server dial default listen",
			req:  SSHInstallRequest{Token: "t", ConnectMode: "server_dial"},
			s:    scriptInstall{installDir: "/opt/gpuworks"},
			want: "--install-dir '/opt/gpuworks' --token 't' --connect-mode 'server_dial' --supervisor 'systemd' --listen '" + agentconf.DefaultListen + "'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.s.conf = tc.req.agentConfig()
			if got := tc.s.args(); got != tc.want {
				t.Fatalf("args =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestAgentStateProbeCommandIsValidShell(t *testing.T) {
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(agentStateProbeCommand("/opt/gpu works"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
