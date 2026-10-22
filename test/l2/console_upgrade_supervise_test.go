//go:build multiproc

package l2

// Where there is no systemd, `gpuworks install --no-systemd` starts the
// server behind `serve --supervise`, which starts it again whenever it exits.
// These cases run the console upgrade under that supervisor rather than the
// stand-in for systemd, both detached (as install leaves it) and in the
// foreground (as a container's main process runs it), and check that a server
// whose supervisor was killed outright does not outlive it.

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConsoleUpgradeUnderTheNoSystemdSupervisor(t *testing.T) {
	archive := releasePackage(t, "v1.2.4", readFileAt(t, buildServerRelease(t, "v1.2.4")))
	fx := installControlPlane(t, "v1.2.3")
	supervisor := readPidFile(t, filepath.Join(fx.dir, "gpuworks.pid"))
	token := loginWithPassword(t, fx.base, controlPlanePassword)

	status, body := consoleUpload(t, fx.base, token, archive)
	if status != http.StatusAccepted || !strings.Contains(body, `"from":"v1.2.3","to":"v1.2.4"`) {
		t.Fatalf("upload = %d %s", status, body)
	}
	assertHealthVersion(t, fx.base, "v1.2.4")
	entries := waitForHistory(t, fx.dir, 2, 45*time.Second)
	if len(entries) != 2 {
		t.Fatalf("install history = %+v", entries)
	}
	if last := entries[1]; last.Action != "upgrade" || last.Result != "ok" || last.ReleaseID != "v1.2.4" || last.Mode != "nohup" {
		t.Fatalf("the console upgrade was not recorded: %+v", last)
	}
	// The supervisor is the one install started: only the server behind it
	// was replaced, and the pid file still names it for the CLI.
	if got := readPidFile(t, filepath.Join(fx.dir, "gpuworks.pid")); got != supervisor || syscall.Kill(supervisor, 0) != nil {
		t.Fatalf("pid file = %d, want the running supervisor %d", got, supervisor)
	}
	loginWithPassword(t, fx.base, controlPlanePassword)
}

// The release fails every start after the guard has counted it (see
// TestConsoleUpgradeRollsBackWhenTheReleaseCannotServe); the supervisor keeps
// starting it until the guard has put the previous version back.
func TestConsoleUpgradeRollsBackUnderTheNoSystemdSupervisor(t *testing.T) {
	real := buildServerRelease(t, "v2.0.0")
	occupied := holdPort(t)
	wrapper := "#!/bin/sh\nif [ \"$1\" = version ]; then exec " + real + " version; fi\nexec " + real + " \"$@\" --addr " + occupied + "\n"
	archive := releasePackage(t, "v2.0.0", []byte(wrapper))
	fx := installControlPlane(t, "v1.2.3")
	token := loginWithPassword(t, fx.base, controlPlanePassword)

	status, body := consoleUpload(t, fx.base, token, archive)
	if status != http.StatusAccepted || !strings.Contains(body, `"to":"v2.0.0"`) {
		t.Fatalf("upload = %d %s", status, body)
	}
	// The old process can still answer during its delayed shutdown. The
	// failed upgrade record proves the guard has actually restored v1.2.3.
	entries := waitForHistory(t, fx.dir, 2, time.Minute)
	if len(entries) != 2 {
		t.Fatalf("install history = %+v", entries)
	}
	if last := entries[1]; last.Action != "upgrade" || last.Result != "failed" || last.ReleaseID != "v2.0.0" ||
		last.PreviousReleaseID != "v1.2.3" || last.Mode != "nohup" || !strings.Contains(last.Error, "did not come up") {
		t.Fatalf("the rollback was not recorded: %+v", last)
	}
	assertHealthVersion(t, fx.base, "v1.2.3")
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, "v1.2.3") {
		t.Fatalf("the installed binary reports %q after the rollback", strings.TrimSpace(got))
	}
}

// A container that restarts kills its processes outright and runs its entry
// point again. Here the detached supervisor is killed with SIGKILL, which
// must take the server with it, and the installed binary is then run in the
// foreground as an entry point would run it: it serves, records itself, takes
// a console upgrade, and stops with the server on SIGTERM.
func TestSupervisorInTheForegroundAfterTheOldOneWasKilled(t *testing.T) {
	archive := releasePackage(t, "v1.2.4", readFileAt(t, buildServerRelease(t, "v1.2.4")))
	fx := installControlPlane(t, "v1.2.3")
	pidPath := filepath.Join(fx.dir, "gpuworks.pid")
	_ = syscall.Kill(readPidFile(t, pidPath), syscall.SIGKILL)
	waitUnanswered(t, fx.base)

	entry := exec.Command(fx.bin, "serve", "--supervise", "--config", filepath.Join(fx.dir, "server.yaml"))
	log, err := os.Create(filepath.Join(t.TempDir(), "entrypoint.log"))
	if err != nil {
		t.Fatal(err)
	}
	entry.Stdout, entry.Stderr = log, log
	if err := entry.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- entry.Wait() }()
	t.Cleanup(func() {
		_ = entry.Process.Kill()
		if t.Failed() {
			t.Logf("entry point output:\n%s", readFileAt(t, log.Name()))
		}
	})
	assertHealthVersion(t, fx.base, "v1.2.3")
	if got := readPidFile(t, pidPath); got != entry.Process.Pid {
		t.Fatalf("pid file = %d, want the foreground supervisor %d", got, entry.Process.Pid)
	}

	token := loginWithPassword(t, fx.base, controlPlanePassword)
	if status, body := consoleUpload(t, fx.base, token, archive); status != http.StatusAccepted {
		t.Fatalf("upload = %d %s", status, body)
	}
	assertHealthVersion(t, fx.base, "v1.2.4")
	if entries := waitForHistory(t, fx.dir, 2, 45*time.Second); len(entries) != 2 || entries[1].Result != "ok" {
		t.Fatalf("install history = %+v", entries)
	}

	_ = entry.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the supervisor exited with %v on SIGTERM", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor did not stop on SIGTERM")
	}
	waitUnanswered(t, fx.base)
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("the stopped supervisor left its pid file: %v", err)
	}
}

func holdPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// waitUnanswered waits until nothing answers on base.
func waitUnanswered(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := processHTTPClient.Get(base + "/healthz")
		if err != nil {
			return
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s still answers; the server outlived its supervisor", base)
}
