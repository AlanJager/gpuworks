//go:build multiproc

package installscript

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

// The real agent installed by the real script for a node the server dials,
// with no server address anywhere: configure writes the file, the agent
// starts as `run --state DIR` and listens where it was told to.
//
// go test -tags=multiproc ./internal/server/installscript -run '^TestScriptStartsARealServerDialAgentFromItsConfiguration$'
func TestScriptStartsARealServerDialAgentFromItsConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	work := t.TempDir()
	binary := filepath.Join(work, "gpuworks-agent.upload")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/gpuworks-agent")
	build.Dir = "../../.."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real agent: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	listen := "127.0.0.1:" + strconv.Itoa(port)
	dir := filepath.Join(work, "gpuworks")

	// --no-sudo: never touch the machine running the test, whatever sudo
	// rights its user has. A user who is not root gets the nohup start.
	cmd := exec.CommandContext(ctx, "sh", "-c", Script, "install.sh",
		"--install-dir", dir, "--token", "node-token", "--connect-mode", "server_dial", "--listen", listen, "--driver", "none", "--no-sudo", "--supervisor", "nohup")
	cmd.Env = append(os.Environ(), "GPUWORKS_AGENT_BINARY="+binary, "GPUWORKS_FAKE_GPU=L4x1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	pidData, err := os.ReadFile(filepath.Join(dir, "gpuworks-agent.pid"))
	if err != nil {
		t.Fatalf("no pid file: %v\n%s", err, out)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })

	conf, err := agentconf.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := (agentconf.Config{Token: "node-token", ConnectMode: "server_dial", Listen: listen, RuntimeDriver: "none"}); conf != want {
		t.Fatalf("configuration = %+v, want %+v", conf, want)
	}
	if info, err := os.Stat(agentconf.Path(dir)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("configuration file mode = %v, %v", info.Mode().Perm(), err)
	}
	var conn net.Conn
	for deadline := time.Now().Add(30 * time.Second); ; {
		if conn, err = net.DialTimeout("tcp", listen, time.Second); err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(dir, "agent.log"))
			t.Fatalf("the agent never listened on %s: %v\n%s", listen, err, log)
		}
		time.Sleep(200 * time.Millisecond)
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " "), filepath.Join(dir, "gpuworks-agent")+" run --state "+dir; got != want {
		t.Fatalf("agent command line = %q, want %q", got, want)
	}
}
