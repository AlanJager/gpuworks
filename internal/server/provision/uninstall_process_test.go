//go:build multiproc || docker

package provision

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// go test -tags=multiproc ./internal/server/provision -run '^TestUninstallAgentSSHStopsRealProcess$'
func TestUninstallAgentSSHStopsRealProcess(t *testing.T) {
	testUninstallAgentProcess(t, false)
}

// go test -tags=docker ./internal/server/provision -run '^TestUninstallAgentSSHStopsContainerProcess$'
func TestUninstallAgentSSHStopsContainerProcess(t *testing.T) {
	testUninstallAgentProcess(t, true)
}

func testUninstallAgentProcess(t *testing.T, container bool) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "gpuworks-agent")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/gpuworks-agent")
	build.Dir = "../../.."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real agent: %v\n%s", err, out)
	}
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity.json", "model-data.bin"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte("retained user data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	remoteBinary, remoteState := binary, state
	var containerID string
	if container {
		image := os.Getenv("GPUWORKS_UNINSTALL_TEST_IMAGE")
		if image == "" {
			image = "python:3.12"
		}
		out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--network=none", "-v", dir+":/fixture", "--entrypoint", "sleep", image, "180").CombinedOutput()
		if err != nil {
			t.Fatalf("start uninstall fixture container: %v\n%s", err, out)
		}
		containerID = strings.TrimSpace(string(out))
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })
		remoteBinary, remoteState = "/fixture/gpuworks-agent", "/fixture/state"
	}
	runShell := func(command string, input io.Reader) (string, string, uint32) {
		args := []string{"sh", "-c", command}
		if container {
			args = append([]string{"docker", "exec", "-i", containerID}, args...)
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin = input
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		var status uint32
		if err := cmd.Run(); err != nil {
			status = 1
			if exit, ok := err.(*exec.ExitError); ok {
				status = uint32(exit.ExitCode())
			}
		}
		return stdout.String(), stderr.String(), status
	}
	start := fmt.Sprintf("nohup %s run --server %s --token uninstall-fixture-token --state %s --driver none > %s 2>&1 & echo $!", shellQuote(remoteBinary), shellQuote(server.URL), shellQuote(remoteState), shellQuote(remoteState+"/console.log"))
	out, stderr, status := runShell(start, nil)
	if status != 0 {
		t.Fatalf("start agent: %s %s (%d)", out, stderr, status)
	}
	pid := strings.TrimSpace(out)
	if value, err := strconv.Atoi(pid); err != nil || value <= 0 {
		t.Fatalf("invalid fixture PID %q", pid)
	}
	if !container {
		t.Cleanup(func() { _ = exec.Command("kill", pid).Run() })
	}
	ready := time.NewTicker(20 * time.Millisecond)
	defer ready.Stop()
	for {
		if _, err := os.Stat(filepath.Join(state, "agent.log")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("agent did not initialize its log")
		case <-ready.C:
		}
	}
	host, port := startFakeSSHServer(t, "root", "pw", runShell)
	installer := NewSSHInstaller(nil)
	if err := installer.UninstallAgent(ctx, SSHProbeRequest{
		Host: host, Port: port, User: "root", ServerURL: server.URL,
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	}); err != nil {
		log, _ := os.ReadFile(filepath.Join(state, "console.log"))
		t.Fatalf("SSH did not confirm real agent exit: %v\n%s", err, log)
	}
	if out, stderr, status := runShell("if [ -r /proc/"+pid+"/cmdline ]; then [ -z \"$(tr '\\000' ' ' < /proc/"+pid+"/cmdline)\" ] || exit 1; fi; echo stopped", nil); status != 0 || strings.TrimSpace(out) != "stopped" {
		t.Fatalf("agent is still executing: %s %s (%d)", out, stderr, status)
	}
	if _, err := os.Stat(filepath.Join(state, "identity.json")); !os.IsNotExist(err) {
		t.Fatalf("identity retained after successful uninstall: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(state, "model-data.bin")); err != nil || string(data) != "retained user data" {
		t.Fatalf("model data changed: %q %v", data, err)
	}
}

// One host, two real agents of the same server: A under the gpuworks-agent
// unit, B under nohup from its own directory. Deleting B's node stops B only;
// A keeps running and its unit stays installed.
//
// go test -tags=multiproc ./internal/server/provision -run '^TestUninstallAgentSSHLeavesTheHostsOtherAgentRunning$'
func TestUninstallAgentSSHLeavesTheHostsOtherAgentRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	type agent struct{ dir, binary, state, token, pid string }
	agents := map[string]*agent{}
	for _, name := range []string{"a", "b"} {
		a := &agent{dir: filepath.Join(dir, name), token: "token-of-" + name}
		a.binary, a.state = filepath.Join(a.dir, "gpuworks-agent"), filepath.Join(a.dir, "state")
		build := exec.CommandContext(ctx, "go", "build", "-o", a.binary, "./cmd/gpuworks-agent")
		build.Dir = "../../.."
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build real agent: %v\n%s", err, out)
		}
		if err := os.MkdirAll(a.state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.state, "identity.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		agents[name] = a
	}
	tools := filepath.Join(dir, "tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	unitPath, calls := filepath.Join(dir, "unit"), filepath.Join(dir, "systemctl.calls")
	unit := "[Service]\nExecStart=" + agents["a"].binary + " run --server " + server.URL + " --token " + agents["a"].token + " --state " + agents["a"].state + " --driver none\n"
	if err := os.WriteFile(unitPath, []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	// Root, as on gpu3: no sudo, which would reset PATH past the stubs.
	writeExecutable(t, filepath.Join(tools, "id"), `echo 0`)
	writeExecutable(t, filepath.Join(tools, "systemctl"), `echo "$*" >> "$CALLS"
case "$*" in
  "cat gpuworks-agent.service") cat "$UNIT" ;;
  *) exit 0 ;;
esac`)
	runShell := func(command string, input io.Reader) (string, string, uint32) {
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Stdin = input
		cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "UNIT="+unitPath, "CALLS="+calls)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		var status uint32
		if err := cmd.Run(); err != nil {
			status = 1
			if exit, ok := err.(*exec.ExitError); ok {
				status = uint32(exit.ExitCode())
			}
		}
		return stdout.String(), stderr.String(), status
	}
	for _, a := range agents {
		start := fmt.Sprintf("nohup %s run --server %s --token %s --state %s --driver none > %s 2>&1 & echo $!",
			shellQuote(a.binary), shellQuote(server.URL), shellQuote(a.token), shellQuote(a.state), shellQuote(a.state+"/console.log"))
		out, stderr, status := runShell(start, nil)
		if status != 0 {
			t.Fatalf("start agent: %s %s (%d)", out, stderr, status)
		}
		a.pid = strings.TrimSpace(out)
		pid := a.pid
		t.Cleanup(func() { _ = exec.Command("kill", pid).Run() })
	}
	for _, a := range agents {
		for {
			if _, err := os.Stat(filepath.Join(a.state, "agent.log")); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("agent did not initialize its log")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	host, port := startFakeSSHServer(t, "root", "pw", runShell)
	if err := NewSSHInstaller(nil).UninstallAgent(ctx, SSHProbeRequest{
		Host: host, Port: port, User: "root", ServerURL: server.URL, Token: agents["b"].token,
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	}); err != nil {
		t.Fatalf("uninstall B: %v", err)
	}
	running := func(pid string) bool {
		data, err := os.ReadFile("/proc/" + pid + "/cmdline")
		return err == nil && len(data) > 0
	}
	if running(agents["b"].pid) {
		t.Fatal("B still runs after its node was deleted")
	}
	if !running(agents["a"].pid) {
		t.Fatal("deleting B stopped A")
	}
	if _, err := os.Stat(filepath.Join(agents["a"].state, "identity.json")); err != nil {
		t.Fatalf("A lost its identity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agents["b"].state, "identity.json")); !os.IsNotExist(err) {
		t.Fatalf("B's identity remains: %v", err)
	}
	if data, _ := os.ReadFile(calls); strings.Contains(string(data), "disable") || strings.Contains(string(data), "daemon-reload") {
		t.Fatalf("A's unit was touched: %s", data)
	}
	if data, err := os.ReadFile(unitPath); err != nil || string(data) != unit {
		t.Fatalf("A's unit changed: %q %v", data, err)
	}
}
