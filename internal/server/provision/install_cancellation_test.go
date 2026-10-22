package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSHHandler answers one exec request: it receives the command text and
// the channel (so it can drain stdin, as uploadStream and the debug-helper
// upload both stream data into the remote shell) and returns the session's
// stdout/stderr/exit status, mirroring real SSH exec semantics faithfully
// enough to exercise cancellation/timeout timing without a real sshd.
type fakeSSHHandler func(cmd string, stdin io.Reader) (stdout, stderr string, status uint32)

// startFakeSSHServer runs a minimal in-process SSH server whose exec sessions
// are routed through handler. It is the real golang.org/x/crypto/ssh wire
// protocol (host key + password auth + exec channels), not a stub of the
// provision package's own types, so it exercises the exact *ssh.Client that
// run/runContext/uploadStream operate on.
func startFakeSSHServer(t *testing.T, user, password string, handler fakeSSHHandler) (host string, port int) {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if meta.User() != user || string(pass) != password {
				return nil, fmt.Errorf("password rejected")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go serveFakeSSHConn(conn, cfg, handler)
		}
	}()
	h, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	portNum, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, portNum
}

func serveFakeSSHConn(conn net.Conn, cfg *ssh.ServerConfig, handler fakeSSHHandler) {
	serverConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "session channels only")
			continue
		}
		channel, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer channel.Close()
			for req := range requests {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
					_ = req.Reply(false, nil)
					return
				}
				_ = req.Reply(true, nil)
				stdout, stderr, status := handler(payload.Command, channel)
				_, _ = channel.Write([]byte(stdout))
				_, _ = channel.Stderr().Write([]byte(stderr))
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{status}))
				return
			}
		}()
	}
}

func dialFakeSSH(t *testing.T, host string, port int, user, password string) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)), &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestUploadStreamCleansUpStagedFileAfterCancelDuringUpload proves the
// ctx.Done() branch's "rm -f <staged>" cleanup still reaches the remote host
// when ctx is already canceled - it must run on a context detached from ctx,
// not reuse the already-done one (which would abort it immediately and leak
// the staged file).
func TestUploadStreamCleansUpStagedFileAfterCancelDuringUpload(t *testing.T) {
	cleanup := make(chan string, 1)
	unblock := make(chan struct{})
	host, port := startFakeSSHServer(t, "tester", "pw", func(cmd string, stdin io.Reader) (string, string, uint32) {
		if strings.HasPrefix(cmd, "rm -f ") {
			cleanup <- cmd
			return "", "", 0
		}
		// Simulate an upload exec that hangs server-side until the test lets
		// it go, so the client's ctx.Done() branch - not the happy path - is
		// what drives the cleanup call.
		<-unblock
		return "", "", 1
	})
	client := dialFakeSSH(t, host, port, "tester", "pw")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- uploadStream(ctx, client, "/opt/gpuworks/gpuworks-agent", []byte("payload"), true, time.Minute, false)
	}()

	cancel()

	select {
	case cmd := <-cleanup:
		if !strings.Contains(cmd, ".new.") {
			t.Fatalf("cleanup command = %q, want it to target the staged file", cmd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled upload did not issue the rm -f cleanup command")
	}
	close(unblock)
	if err := <-done; err == nil {
		t.Fatal("uploadStream should report the cancellation, not silently succeed")
	}
}

// TestInstallSSHRunsStartCommandToCompletionAfterReplaceBegins proves the
// "point of no return" in InstallSSH: once the atomic binary replace starts,
// canceling the caller's ctx (a browser disconnect via r.Context(), or the
// server's shutdown ctx via recovery.go) must not tear down the SSH session
// before the start command runs - the install must still report its real,
// non-cancellation outcome.
func TestInstallSSHRunsStartCommandToCompletionAfterReplaceBegins(t *testing.T) {
	replaceStarted := make(chan struct{})
	unblockReplace := make(chan struct{})
	startCommandSeen := make(chan string, 1)
	verifySeen := make(chan struct{}, 1)

	host, port := startFakeSSHServer(t, "tester", "pw", func(cmd string, stdin io.Reader) (string, string, uint32) {
		switch {
		// Exact match: other commands contain "id -u" as a substring.
		case cmd == "id -u":
			return "0\n", "", 0 // root: no sudo path, keeps the fixture minimal
		case strings.HasPrefix(cmd, "uname -m"):
			return "x86_64\n", "", 0
		case strings.HasPrefix(cmd, "mv -f "):
			close(replaceStarted)
			<-unblockReplace
			return "", "", 0
		case strings.HasPrefix(cmd, "env GPUWORKS_AGENT_BINARY="):
			startCommandSeen <- cmd
			return "started via nohup pid 123\n", "", 0
		case strings.HasPrefix(cmd, "set +e"):
			verifySeen <- struct{}{}
			return "systemd\n", "", 0
		case strings.HasPrefix(cmd, `sh -c 'cd "$HOME"`):
			return "/home/tester\n", "", 0
		case strings.HasPrefix(cmd, "mkdir -p"):
			// Both the agent binary upload and the udocker debug-helper
			// upload land here; just drain stdin so the client's write
			// completes, mirroring a real shell consuming the payload.
			_, _ = io.Copy(io.Discard, stdin)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	installer := &SSHInstaller{
		Timeout: 5 * time.Second,
		ReadBin: func(string) ([]byte, error) { return []byte("agent-binary-contents"), nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		report SSHInstallReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := installer.InstallSSH(ctx, SSHInstallRequest{
			Target:    fmt.Sprintf("tester@%s:%d", host, port),
			ServerURL: "https://server.example",
			Token:     "node-token",
			SSHAuth:   SSHAuth{Password: "pw"},
		})
		done <- result{report, err}
	}()

	select {
	case <-replaceStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("InstallSSH never reached the atomic replace step")
	}
	// Past the point of no return: cancel the caller's ctx the way a browser
	// disconnect or server shutdown would, then let the replace command
	// finish. If the fix regressed, the remaining steps would observe a
	// canceled context and InstallSSH would return a cancellation error
	// instead of running the start/verify commands.
	cancel()
	close(unblockReplace)

	select {
	case cmd := <-startCommandSeen:
		if !strings.Contains(cmd, "--server 'https://server.example'") {
			t.Fatalf("start command = %q, want install.sh run with the server URL", cmd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start command never ran after ctx was canceled past the point of no return")
	}
	select {
	case <-verifySeen:
	case <-time.After(5 * time.Second):
		t.Fatal("verify command never ran after ctx was canceled past the point of no return")
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("InstallSSH returned an error despite the cancellation happening after the point of no return: %v\nchecks: %+v", res.err, res.report.Checks)
	}
}
