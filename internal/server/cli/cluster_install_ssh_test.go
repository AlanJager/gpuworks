package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestClusterSSHDeadlineReleasesInheritedProxyStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a Unix shell and OpenSSH")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is unavailable")
	}
	private := t.TempDir()
	ready, finished := filepath.Join(private, "ready"), filepath.Join(private, "finished")
	script := filepath.Join(private, "proxy.sh")
	// OpenSSH supplies its stderr to ProxyCommand. Keep that descriptor open
	// after cancellation kills ssh, without making any network connection.
	// The child has a short fixed lifetime; cleanup waits for its completion.
	source := "#!/bin/sh\nset -eu\nprintf 'proxy started\\n' >&2\n: > " + clusterInstallQuote(ready) + "\nsleep 3\n: > " + clusterInstallQuote(finished) + "\n"
	if err := os.WriteFile(script, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := clusterInstallSSHCommand(ctx, []string{"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "ProxyCommand=sh " + clusterInstallQuote(script), "192.0.2.254", "true"})
	if cmd.WaitDelay <= 0 {
		t.Fatal("production SSH commands must bound inherited output pipes")
	}
	cmd.WaitDelay = 100 * time.Millisecond
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		// Never signal a guessed process or leave the bounded proxy running.
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(finished); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("the private bounded ProxyCommand did not finish")
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("SSH exited before starting the private proxy: %v: %s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("OpenSSH did not start the private ProxyCommand")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("canceled SSH falsely succeeded: %v, context: %v", err, ctx.Err())
		}
		if !bytes.Contains(stderr.Bytes(), []byte("proxy started")) {
			t.Fatalf("the proxy did not retain the buffered stderr pipe: %q", stderr.String())
		}
		if _, err := os.Stat(finished); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SSH waited for the proxy's full lifetime instead of bounding its output pipe: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("canceled SSH remained blocked on ProxyCommand's inherited stderr")
	}
}
