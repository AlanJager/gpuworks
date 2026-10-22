package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/commandlog"
)

func TestRecoveryProbeWithSpacedInstallDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gpu works")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gpuworks-agent.conf"), []byte("SERVER=http://gw\nTOKEN=target-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := dir + "/gpuworks-agent run --state " + dir
	unit := "{ path=" + dir + "/gpuworks-agent ; argv[]=" + line + " ; ignore_errors=no ; pid=123 ; }"
	other := "{ argv[]=/opt/other/gpuworks-agent run --server http://other --token other-token --state /opt/other ; ignore_errors=no ; }"
	for _, tc := range []struct {
		name, unit string
	}{
		{"own systemd", unit},
		{"nohup without other unit", ""},
		{"nohup beside other unit", other},
	} {
		for _, running := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/running=%v", tc.name, running), func(t *testing.T) {
				var lines []string
				activeUnit, want := tc.unit, "not-running"
				if running {
					lines, want = []string{line}, "process"
					if tc.unit == unit {
						want = "systemd"
					}
				} else if tc.unit == unit {
					activeUnit = ""
				}
				out, err := runProbeScript(t, agentRecoveryProbeCommand(dir, "http://gw", "target-token"), lines, activeUnit)
				if err != nil || out != want {
					t.Fatalf("probe=%q err=%v, want=%q", out, err, want)
				}
			})
		}
	}
	for _, unit := range []string{"", unit} {
		out, err := runProbeScript(t, agentRecoveryProbeCommand(dir, "http://gw", "wrong-token"), []string{line}, unit)
		if err == nil || !strings.Contains(out, "ownership") {
			t.Fatalf("conflicting identity must fail: %q %v", out, err)
		}
	}
}

func TestRecoveryProbeMatchesProcessIdentityAndDirectory(t *testing.T) {
	dir := t.TempDir()
	line := dir + "/gpuworks-agent run --state " + dir
	for _, tc := range []struct {
		name, token, server, process, want string
		wantErr                            bool
	}{
		{name: "configured target", token: "target-token", server: "http://gw", process: line, want: "process"},
		{name: "busybox target", token: "target-token", server: "http://gw", process: "  812 root S " + line, want: "process"},
		{name: "other token in target directory", token: "another-token", server: "http://gw", process: line, wantErr: true},
		{name: "missing identity", server: "http://gw", process: line, wantErr: true},
		{name: "missing server", token: "target-token", process: line, wantErr: true},
		{name: "other state directory", process: dir + "/gpuworks-agent run --state " + dir + "-other", want: "not-running"},
		{name: "binary suffix", process: dir + "/gpuworks-agent-other run --state " + dir, want: "not-running"},
		{name: "binary prefix", process: "/other" + line, want: "not-running"},
		{name: "shell argument is not an agent", process: "/bin/sh -c " + line, want: "not-running"},
		{name: "other subcommand", process: dir + "/gpuworks-agent doctor --state " + dir, want: "not-running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := "SERVER=" + tc.server + "\nTOKEN=" + tc.token + "\n"
			if err := os.WriteFile(filepath.Join(dir, "gpuworks-agent.conf"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runProbeScript(t, agentRecoveryProbeCommand(dir, "http://gw", "target-token"), []string{tc.process}, "")
			if (err != nil) != tc.wantErr || !tc.wantErr && out != tc.want {
				t.Fatalf("probe=%q err=%v, want=%q error=%v", out, err, tc.want, tc.wantErr)
			}
			for _, token := range []string{"target-token", "another-token"} {
				if strings.Contains(out, token) {
					t.Fatal("probe exposed a node token")
				}
			}
		})
	}
}

func TestRecoveryProbeChecksConfiguredUnitOwnership(t *testing.T) {
	dir, otherDir := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		name, state, token, want string
		wantErr                  bool
	}{
		{name: "own unit", state: dir, token: "target-token", want: "systemd"},
		{name: "other unit", state: otherDir, token: "other-token", want: "not-running"},
		{name: "other unit unreadable identity", state: otherDir, want: "not-running"},
		{name: "target directory conflicting identity", state: dir, token: "other-token", wantErr: true},
		{name: "target directory unreadable identity", state: dir, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(tc.state, "gpuworks-agent.conf"), []byte("SERVER=http://gw\nTOKEN="+tc.token+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			unit := "{ argv[]=" + tc.state + "/gpuworks-agent run --state " + tc.state + " ; }"
			out, err := runProbeScript(t, agentRecoveryProbeCommand(dir, "http://gw", "target-token"), nil, unit)
			if (err != nil) != tc.wantErr || !tc.wantErr && out != tc.want {
				t.Fatalf("probe=%q err=%v, want=%q error=%v", out, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestRecoveryProbeRequiresIdentityAndRedactsItInLogs(t *testing.T) {
	if out, err := runProbeScript(t, agentRecoveryProbeCommand("/opt/gpuworks", "http://gw", ""), nil, ""); err == nil || !strings.Contains(out, "node token is required") {
		t.Fatalf("missing identity: %q %v", out, err)
	}
	const token = "secret-node-identity"
	if strings.Contains(commandlog.Redact(agentRecoveryProbeCommand("/opt/gpuworks", "http://gw", token)), token) {
		t.Fatal("SSH command logging exposes the node token")
	}
}
