//go:build multiproc

package l2

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

func TestSSHInstallUploadsVerifiesAndAtomicallyReplacesMissingDirectory(t *testing.T) {
	var mu sync.Mutex
	var uploads [][]byte
	var uploadCommands []string
	host, port := startSSHSessionFixture(t, "root", "password", func(command string, channel ssh.Channel) (string, string, uint32) {
		switch {
		case command == "id -u":
			return "0\n", "", 0
		case strings.Contains(command, "uname -m"):
			return "x86_64\n", "", 0
		case strings.Contains(command, "cat >") && strings.Contains(command, "sha256sum -c"):
			data, err := io.ReadAll(channel)
			if err != nil {
				return "", err.Error(), 1
			}
			mu.Lock()
			uploads = append(uploads, append([]byte(nil), data...))
			uploadCommands = append(uploadCommands, command)
			mu.Unlock()
			return "staged: OK\n", "", 0
		case strings.Contains(command, "systemctl is-active") && strings.Contains(command, "ps axww"):
			return "process\n", "", 0
		case strings.Contains(command, `cd "$HOME"`):
			return "/root\n", "", 0
		default:
			return "ok\n", "", 0
		}
	})

	binary := []byte("gpuworks-agent-fixture")
	installer := provision.NewSSHInstaller(func(name string) ([]byte, error) {
		if name != "gpuworks-agent-linux-amd64" {
			t.Fatalf("binary name=%q", name)
		}
		return binary, nil
	})
	installer.Timeout = 2 * time.Second
	installer.UploadTimeout = time.Second
	var phases []string
	report, err := installer.InstallSSH(context.Background(), provision.SSHInstallRequest{
		Target: net.JoinHostPort(host, stringPort(port)), User: "root",
		ServerURL: "http://server.example", Token: "token", InstallDir: "/ephemeral/new/gpuworks",
		OnPhase: func(phase string) error { phases = append(phases, phase); return nil },
		SSHAuth: provision.SSHAuth{Password: "password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.InstallDir != "/ephemeral/new/gpuworks" || len(uploads) < 1 {
		t.Fatalf("report=%+v uploads=%q", report, uploads)
	}
	// The installer gzips the wire: the captured stream must decompress back
	// to the exact binary bytes.
	gz, err := gzip.NewReader(bytes.NewReader(uploads[0]))
	if err != nil {
		t.Fatalf("upload is not a gzip stream: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("decompressed upload = %q, want %q", got, binary)
	}
	wantPhases := []string{"artifact_check", "uploading", "verifying", "replacing", "restarting"}
	if !reflect.DeepEqual(phases, wantPhases) {
		t.Fatalf("phases=%v want=%v", phases, wantPhases)
	}
	if !strings.Contains(uploadCommands[0], "mkdir -p '/ephemeral/new/gpuworks'") || !strings.Contains(uploadCommands[0], "sha256sum -c -") {
		t.Fatalf("upload command does not create and verify destination: %s", uploadCommands[0])
	}
	if !strings.Contains(uploadCommands[0], "gunzip -c") || strings.Contains(uploadCommands[0], "sha256sum -c - && gunzip") {
		t.Fatalf("upload command must decompress before the digest check: %s", uploadCommands[0])
	}
}

func TestSSHInstallUploadTimeoutClosesSessionAndCleansStagedFile(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	host, port := startSSHSessionFixture(t, "root", "password", func(command string, _ ssh.Channel) (string, string, uint32) {
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		switch {
		case command == "id -u":
			return "0\n", "", 0
		case strings.Contains(command, "uname -m"):
			return "x86_64\n", "", 0
		case strings.Contains(command, "cat >"):
			time.Sleep(300 * time.Millisecond)
			return "", "", 0
		case strings.Contains(command, "mkdir -p '/tmp/gpuworks'"):
			return "", "fallback disabled", 1
		default:
			return "", "", 0
		}
	})
	installer := provision.NewSSHInstaller(func(string) ([]byte, error) { return make([]byte, 256<<10), nil })
	installer.Timeout = 2 * time.Second
	installer.UploadTimeout = 30 * time.Millisecond
	_, err := installer.InstallSSH(context.Background(), provision.SSHInstallRequest{
		Target: net.JoinHostPort(host, stringPort(port)), User: "root",
		ServerURL: "http://server.example", Token: "token", InstallDir: "/ephemeral/gpuworks",
		SSHAuth: provision.SSHAuth{Password: "password"},
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 30ms") {
		t.Fatalf("timeout error=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !containsCommand(commands, "rm -f '/ephemeral/gpuworks/gpuworks-agent.new.") {
		t.Fatalf("staged cleanup missing from commands: %v", commands)
	}
}

func TestSSHInstallSHAMismatchDoesNotReplaceFinalBinary(t *testing.T) {
	var mu sync.Mutex
	var commands []string
	host, port := startSSHSessionFixture(t, "root", "password", func(command string, channel ssh.Channel) (string, string, uint32) {
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		switch {
		case command == "id -u":
			return "0\n", "", 0
		case strings.Contains(command, "uname -m"):
			return "x86_64\n", "", 0
		case strings.Contains(command, "cat >"):
			_, _ = io.ReadAll(channel)
			return "", "sha256sum: WARNING: checksum did not match", 1
		case strings.Contains(command, "mkdir -p '/tmp/gpuworks'"):
			return "", "fallback disabled", 1
		default:
			return "", "", 0
		}
	})
	installer := provision.NewSSHInstaller(func(string) ([]byte, error) { return []byte("corrupt-me"), nil })
	installer.Timeout = 2 * time.Second
	installer.UploadTimeout = time.Second
	_, err := installer.InstallSSH(context.Background(), provision.SSHInstallRequest{
		Target: net.JoinHostPort(host, stringPort(port)), User: "root",
		ServerURL: "http://server.example", Token: "token", InstallDir: "/ephemeral/gpuworks",
		SSHAuth: provision.SSHAuth{Password: "password"},
	})
	if err == nil || !strings.Contains(err.Error(), "checksum did not match") {
		t.Fatalf("sha mismatch error=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, command := range commands {
		if strings.HasPrefix(command, "mv -f '/ephemeral/gpuworks/gpuworks-agent.new.") && strings.HasSuffix(command, " '/ephemeral/gpuworks/gpuworks-agent'") {
			t.Fatalf("final binary was replaced after sha mismatch: %s", command)
		}
	}
	if !containsCommand(commands, "rm -f '/ephemeral/gpuworks/gpuworks-agent.new.") {
		t.Fatalf("staged cleanup missing after sha mismatch: %v", commands)
	}
}

func containsCommand(commands []string, fragment string) bool {
	for _, command := range commands {
		if strings.Contains(command, fragment) {
			return true
		}
	}
	return false
}

func stringPort(port int) string {
	return strconv.Itoa(port)
}
