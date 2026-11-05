package provision

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionDiagnosticsNodeSideHTTPStages(t *testing.T) {
	for _, tc := range []struct{ name, exit, status, outcome string }{
		{"healthy endpoint", "0", "200", "http_ok"},
		{"DNS", "6", "000", "dns"},
		{"connect", "7", "000", "connect"},
		{"TLS", "60", "000", "tls"},
		{"timeout", "28", "000", "timeout"},
		{"HTTP loading", "0", "503", "http_error"},
		{"redirect is not followed", "0", "302", "http_error"},
		{"other transport", "52", "000", "transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, bin := connectionDiagnosticFixture(t, "wanted-token", "https://old.example/base")
			writeExecutable(t, filepath.Join(bin, "curl"), fmt.Sprintf(`
[ "$#" -eq 15 ] || exit 99
[ "$1" = --silent ] && [ "$2" = --output ] && [ "$3" = /dev/null ] || exit 99
[ "$4" = --write-out ] && [ "$5" = '%%{http_code}' ] || exit 99
[ "$6" = --connect-timeout ] && [ "$7" = 3 ] || exit 99
[ "$8" = --max-time ] && [ "$9" = 5 ] || exit 99
shift 9
[ "$1" = --max-redirs ] && [ "$2" = 0 ] && [ "$3" = --proto ] && [ "$4" = '=http,https' ] || exit 99
[ "$5" = -- ] && [ "$6" = https://old.example/base/healthz ] || exit 99
printf '%%s' '%s'
printf 'password=must-not-be-logged' >&2
exit %s
`, tc.status, tc.exit))
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, input io.Reader) (string, string, uint32) {
				token, _ := io.ReadAll(input)
				if string(token) != "wanted-token\n" || strings.Contains(cmd, "wanted-token") {
					t.Error("ownership token must be passed on stdin only")
				}
				return executeConnectionDiagnostic(t, cmd, token, bin)
			})
			i := NewSSHInstaller(nil)
			got := i.DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "wanted-token", ServerURL: "https://old.example/base", InstallDir: dir, SSHAuth: SSHAuth{Password: "pw"}})
			if got.SSH.Outcome != "connected" || got.Configuration.Outcome != "snapshot" || got.Configuration.Source != "state_file" || got.Configuration.Target != "https://old.example/base" {
				t.Fatalf("node configuration: %+v", got)
			}
			if got.Health.Outcome != tc.outcome || got.Health.Target != "https://old.example/base/healthz" || got.Health.ObservedAt.IsZero() {
				t.Fatalf("node HTTP observation: %+v", got.Health)
			}
			if strings.Contains(logs.String(), "wanted-token") || strings.Contains(logs.String(), "must-not-be-logged") {
				t.Fatalf("diagnostic log leaked credentials: %s", logs.String())
			}
		})
	}
}

func TestConnectionDiagnosticsRequireNodeIdentity(t *testing.T) {
	dir, bin := connectionDiagnosticFixture(t, "another-node-token", "https://other.example")
	marker := filepath.Join(t.TempDir(), "curl-called")
	writeExecutable(t, filepath.Join(bin, "curl"), "touch "+shellQuote(marker))
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, input io.Reader) (string, string, uint32) {
		token, _ := io.ReadAll(input)
		return executeConnectionDiagnostic(t, cmd, token, bin)
	})
	got := NewSSHInstaller(nil).DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "target-node-token", ServerURL: "https://saved.example", InstallDir: dir, SSHAuth: SSHAuth{Password: "pw"}})
	if got.Configuration.Outcome != "unverified_identity" || got.Configuration.Target != "" || got.Health.Outcome != "not_checked" {
		t.Fatalf("foreign configuration escaped its identity boundary: %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a foreign agent must not be probed")
	}
}

func TestConnectionDiagnosticsRejectUnsafeSavedAddressBeforeSSH(t *testing.T) {
	for _, address := range []string{"https://user:password@example.test", "https://example.test?secret=private", "https://example.test/#token", "file:///etc/passwd"} {
		got := NewSSHInstaller(nil).DiagnoseConnection(context.Background(), SSHProbeRequest{ServerURL: address, Token: "target-token"})
		if got.Health.Outcome != "invalid_address" || got.Health.Target != "" || got.SSH.Outcome != "unavailable" {
			t.Fatalf("unsafe stored address not rejected: %+v", got)
		}
	}
}

func TestConnectionDiagnosticsUntrustedOutputIsBoundedAndNotLogged(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	for _, exit := range []uint32{0, 1} {
		host, port := startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) {
			return strings.Repeat("token=private-node-token password=secret https://user:pw@host/?query-secret=x\n", 10000), "private-key-secret", exit
		})
		got := NewSSHInstaller(nil).DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "private-node-token", ServerURL: "https://saved.example", SSHAuth: SSHAuth{Password: "pw"}})
		if got.Configuration.Outcome != "invalid_response" && got.Configuration.Outcome != "command_failed" {
			t.Fatalf("unsafe remote response accepted: %+v", got)
		}
	}
	for _, secret := range []string{"private-node-token", "query-secret", "private-key-secret", "password=secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("untrusted remote output reached command logs: %s", logs.String())
		}
	}
	var output connectionDiagnosticOutput
	data := []byte(strings.Repeat("x", 100000))
	if n, err := output.Write(data); n != len(data) || err != nil || output.text.Len() > connectionDiagnosticLimit || !output.overflow {
		t.Fatalf("output limit did not bound memory: n=%d err=%v size=%d", n, err, output.text.Len())
	}
}

func TestConnectionDiagnosticsCancellationDoesNotRestart(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, input io.Reader) (string, string, uint32) {
		calls.Add(1)
		if strings.Contains(cmd, "systemctl restart") || strings.Contains(cmd, "install.sh") {
			t.Error("read-only diagnostic attempted mutation")
		}
		_, _ = io.ReadAll(input)
		close(started)
		<-release
		return "password=late-secret", "", 1
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan ConnectionDiagnostic, 1)
	go func() {
		done <- NewSSHInstaller(nil).DiagnoseConnection(ctx, SSHProbeRequest{Host: host, Port: port, User: "root", Token: "node-token", ServerURL: "https://saved.example", SSHAuth: SSHAuth{Password: "pw"}})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("SSH command did not start")
	}
	cancel()
	var got ConnectionDiagnostic
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSH command cancellation did not complete")
	}
	if got.Configuration.Outcome != "cancelled" {
		t.Fatalf("cancellation was not represented: %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("diagnosis did not remain bounded: calls=%d", calls.Load())
	}
}

func TestConnectionDiagnosticsQuotesPersistedTargetsWithoutShellExpansion(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	server := "https://saved.example/$(touch${IFS}" + marker + ")"
	dir, bin := connectionDiagnosticFixture(t, "target-token", "https://saved.example")
	writeExecutable(t, filepath.Join(bin, "curl"), "printf 200")
	host, port := startFakeSSHServer(t, "root", "pw", func(command string, input io.Reader) (string, string, uint32) {
		data, _ := io.ReadAll(input)
		return executeConnectionDiagnostic(t, command, data, bin)
	})
	got := NewSSHInstaller(nil).DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "target-token", ServerURL: server, InstallDir: dir, SSHAuth: SSHAuth{Password: "pw"}})
	if got.Health.Outcome != "http_ok" {
		t.Fatalf("literal target failed: %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("saved URL executed a shell expansion")
	}
}

func TestConnectionDiagnosticsPreserveEnabledHostKeyVerification(t *testing.T) {
	old := verifyHostKeys
	verifyHostKeys = true
	t.Cleanup(func() { verifyHostKeys = old })
	var called atomic.Int32
	host, port := startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) {
		called.Add(1)
		return "", "", 0
	})
	keys := newMemoryHostKeyStore()
	endpoint := net.JoinHostPort(host, strconv.Itoa(port))
	if err := keys.Put(endpoint, "previous-host-fingerprint"); err != nil {
		t.Fatal(err)
	}
	i := NewSSHInstaller(nil)
	i.HostKeys = keys
	got := i.DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "target-token", ServerURL: "https://saved.example", SSHAuth: SSHAuth{Password: "pw"}})
	if got.SSH.Outcome != "ssh_failed" || called.Load() != 0 {
		t.Fatalf("changed host key was not refused: %+v calls=%d", got, called.Load())
	}
	if fingerprint, _ := keys.Get(endpoint); fingerprint != "previous-host-fingerprint" {
		t.Fatal("existing pinned key changed")
	}
}

func TestConnectionDiagnosticsLegacyArgumentsAndUnknownConfig(t *testing.T) {
	dir, bin := connectionDiagnosticFixture(t, "unused", "https://unused.example")
	if err := os.Remove(filepath.Join(dir, "gpuworks-agent.conf")); err != nil {
		t.Fatal(err)
	}
	line := dir + "/gpuworks-agent run --state " + dir + " --token target-token --server https://legacy.example"
	writeExecutable(t, filepath.Join(bin, "ps"), "printf '%s\\n' "+shellQuote(line))
	writeExecutable(t, filepath.Join(bin, "curl"), "printf 200")
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, input io.Reader) (string, string, uint32) {
		token, _ := io.ReadAll(input)
		return executeConnectionDiagnostic(t, cmd, token, bin)
	})
	got := NewSSHInstaller(nil).DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "target-token", ServerURL: "https://saved.example", InstallDir: dir, SSHAuth: SSHAuth{Password: "pw"}})
	if got.Configuration.Source != "legacy_arguments" || got.Configuration.Target != "https://legacy.example" || got.Health.Outcome != "http_ok" {
		t.Fatalf("legacy snapshot=%+v", got)
	}
	writeExecutable(t, filepath.Join(bin, "ps"), "exit 0")
	got = NewSSHInstaller(nil).DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "target-token", ServerURL: "https://saved.example", InstallDir: dir, SSHAuth: SSHAuth{Password: "pw"}})
	if got.Configuration.Outcome != "unavailable" || got.Health.Outcome != "not_checked" {
		t.Fatalf("unknown identity must remain untested: %+v", got)
	}
}

func TestConnectionDiagnosticsMissingToolAndMalformedConfigDoNotLeak(t *testing.T) {
	dir, bin := connectionDiagnosticFixture(t, "target-token", "https://user:private-password@host?token=private-query")
	for _, tool := range []string{"sh", "sed", "tail"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	host, port := startFakeSSHServer(t, "root", "pw", func(command string, input io.Reader) (string, string, uint32) {
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = []string{"PATH=" + bin}
		cmd.Stdin = input
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), "", 1
		}
		return string(out), "", 0
	})
	got := NewSSHInstaller(nil).DiagnoseConnection(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "root", Token: "target-token", ServerURL: "https://saved.example", InstallDir: dir, SSHAuth: SSHAuth{Password: "pw"}})
	if got.Configuration.Target != "" || got.Configuration.Outcome != "invalid_address" || got.Health.Outcome != "tool_missing" {
		t.Fatalf("bad address/tool scope=%+v", got)
	}
}

func connectionDiagnosticFixture(t *testing.T, token, server string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gpuworks-agent.conf"), []byte("TOKEN="+token+"\nSERVER="+server+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "systemctl"), "exit 1")
	return dir, bin
}

func executeConnectionDiagnostic(t *testing.T, command string, input []byte, bin string) (string, string, uint32) {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), stderr.String(), 1
	}
	return string(out), stderr.String(), 0
}
