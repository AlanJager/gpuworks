package docker

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExecPTYBindsValidatedLocalEndpoint(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	for _, contextName := range []string{"", "default"} {
		t.Run("context="+contextName, func(t *testing.T) {
			t.Setenv("DOCKER_CONTEXT", contextName)
			for _, command := range [][]string{nil, {"printf", "%s", "literal with spaces"}} {
				args, err := New().ExecPTY(context.Background(), "  container-123  ", command)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"docker", "--host", "unix://" + sock, "exec", "-i", "-t", "container-123"}
				if command == nil {
					want = append(want, "/bin/sh", "-l")
				} else {
					want = append(want, command...)
				}
				// Terminal.Open constructs a fresh environment without DOCKER_HOST.
				// The argv must retain the selected endpoint independently of it.
				if !slices.Equal(args, want) {
					t.Fatalf("ExecPTY args=%v, want %v", args, want)
				}
			}
		})
	}
}

func TestExecPTYRetainsRelativeEndpointAcrossTerminalWorkingDirectory(t *testing.T) {
	sock := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Chdir(filepath.Dir(sock))
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Base(sock))
	installFakeDocker(t, `[ "$1" = --host ]; endpoint="${2#unix://}"; test -S "$endpoint"; printf '%s' "$2"`)
	args, err := New().ExecPTY(context.Background(), "container-123", nil)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(args[0], args[1:]...)
	command.Dir = t.TempDir() // Terminal.Open chooses its own directory.
	command.Env = []string{"PATH=/usr/bin:/bin"}
	got, err := command.CombinedOutput()
	if err != nil || string(got) != "unix://"+sock {
		t.Fatalf("terminal endpoint changed with cwd: output=%q, err=%v", got, err)
	}
}

func TestExecPTYRejectsUnavailableOrRemoteEndpoint(t *testing.T) {
	notSocket := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(notSocket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, host, context string }{
		{"tcp", "tcp://127.0.0.1:2375", ""},
		{"ssh", "ssh://example.invalid", "default"},
		{"missing", "unix://" + filepath.Join(t.TempDir(), "missing.sock"), ""},
		{"not-socket", "unix://" + notSocket, ""},
		{"non-default-context", "", "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", test.host)
			t.Setenv("DOCKER_CONTEXT", test.context)
			args, err := New().ExecPTY(context.Background(), "container-123", nil)
			if err == nil || args != nil || !strings.Contains(err.Error(), "local unix socket") {
				t.Fatalf("ExecPTY endpoint unexpectedly accepted: args=%v, err=%v", args, err)
			}
		})
	}
}

func TestExecPTYRejectsEmptyContainerID(t *testing.T) {
	if _, err := New().ExecPTY(context.Background(), "  ", nil); err == nil {
		t.Fatal("ExecPTY accepted an empty container id")
	}
}
