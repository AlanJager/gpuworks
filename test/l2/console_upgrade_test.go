//go:build multiproc

package l2

// A console upgrade (ADR 0040) is the running server replacing itself: it
// stages the uploaded release, snapshots its live database, swaps the binary
// and exits for the service manager to start the new one, which either
// confirms the upgrade or, after failing to come up, restores the snapshot.
// These cases run the installed binaries under a stand-in for systemd's
// Restart=always so every step is a real process start.

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// supervisor restarts `<bin> serve` whenever it exits, as systemd does for the
// unit install writes. INVOCATION_ID is what tells the server a service
// manager will bring it back.
type supervisor struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	starts  int
	done    chan struct{}
}

func supervise(t *testing.T, fx controlPlane) *supervisor {
	t.Helper()
	stopInstalledControlPlane(t, fx.dir)
	log, err := os.Create(filepath.Join(t.TempDir(), "supervised.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			s.mu.Lock()
			if s.stopped {
				s.mu.Unlock()
				return
			}
			cmd := exec.Command(fx.bin, "serve", "--config", filepath.Join(fx.dir, "server.yaml"))
			cmd.Env = append(os.Environ(), "INVOCATION_ID=l2-console-upgrade")
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				s.mu.Unlock()
				time.Sleep(500 * time.Millisecond)
				continue
			}
			s.cmd = cmd
			s.starts++
			s.mu.Unlock()
			_ = cmd.Wait()
			time.Sleep(300 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		s.mu.Lock()
		s.stopped = true
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		s.mu.Unlock()
		<-s.done
		_ = log.Close()
		if t.Failed() {
			if out, err := os.ReadFile(log.Name()); err == nil {
				t.Logf("supervised server output:\n%s", out)
			}
		}
	})
	return s
}

func (s *supervisor) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// consoleUpload posts a release archive to the console upgrade endpoint.
func consoleUpload(t *testing.T, base, token string, archive []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/updates/server/upload", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	return resp.StatusCode, body.String()
}

// waitForHistory waits until the install history has n entries.
func waitForHistory(t *testing.T, dir string, n int, budget time.Duration) []historyLine {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		entries := readInstalledHistory(t, dir)
		if len(entries) >= n || time.Now().After(deadline) {
			return entries
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestConsoleUpgradeReplacesTheRelease(t *testing.T) {
	archive := releasePackage(t, "v1.2.4", readFileAt(t, buildServerRelease(t, "v1.2.4")))
	fx := installControlPlane(t, "v1.2.3")
	s := supervise(t, fx)
	assertHealthVersion(t, fx.base, "v1.2.3")
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
	backup := singleBackup(t, fx.dir)
	if last := entries[1]; last.Action != "upgrade" || last.Result != "ok" || last.ReleaseID != "v1.2.4" ||
		last.PreviousReleaseID != "v1.2.3" || last.Backup != backup {
		t.Fatalf("the console upgrade was not recorded: %+v", last)
	}
	// The snapshot is the CLI's format, so `gpuworks upgrade` and an operator
	// restore it the same way.
	members := strings.Join(archiveMembers(t, backup), ",")
	for _, want := range []string{"bin/gpuworks", "data/gpuworks.db", "data/master.key", "server.yaml"} {
		if !strings.Contains(members, want) {
			t.Fatalf("backup members = %s, missing %s", members, want)
		}
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "tmp", "upgrade-pending.json")); !os.IsNotExist(err) {
		t.Fatalf("the upgrade marker survived the confirmation: %v", err)
	}
	// The database survived the swap: the admin still logs in.
	loginWithPassword(t, fx.base, controlPlanePassword)
	if got := s.startCount(); got != 2 {
		t.Fatalf("starts = %d, want the original and the upgraded one", got)
	}
}

// A release that answers `version` but cannot serve is let through by the
// staging check; it is the start guard that brings the previous version back.
func TestConsoleUpgradeRollsBackWhenTheReleaseCannotServe(t *testing.T) {
	real := buildServerRelease(t, "v2.0.0")
	// The wrapper is the binary being installed: its `version` is the real
	// build's, but it serves on a port this test holds, so every start fails
	// after the guard has counted it. A privileged port would not do: CI runs
	// as root, which binds it and the release comes up.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	wrapper := "#!/bin/sh\nif [ \"$1\" = version ]; then exec " + real + " version; fi\nexec " + real + " \"$@\" --addr " + occupied.Addr().String() + "\n"
	archive := releasePackage(t, "v2.0.0", []byte(wrapper))
	fx := installControlPlane(t, "v1.2.3")
	s := supervise(t, fx)
	assertHealthVersion(t, fx.base, "v1.2.3")
	token := loginWithPassword(t, fx.base, controlPlanePassword)

	status, body := consoleUpload(t, fx.base, token, archive)
	if status != http.StatusAccepted || !strings.Contains(body, `"to":"v2.0.0"`) {
		t.Fatalf("upload = %d %s", status, body)
	}
	// The old process can still answer after upload. Wait for the guard's
	// rollback record before checking that the restored release can serve.
	entries := waitForHistory(t, fx.dir, 2, time.Minute)
	if len(entries) != 2 {
		t.Fatalf("install history = %+v", entries)
	}
	if last := entries[1]; last.Action != "upgrade" || last.Result != "failed" || last.ReleaseID != "v2.0.0" ||
		last.PreviousReleaseID != "v1.2.3" || !strings.Contains(last.Error, "did not come up") {
		t.Fatalf("the rollback was not recorded: %+v", last)
	}
	assertHealthVersion(t, fx.base, "v1.2.3")
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, "v1.2.3") {
		t.Fatalf("the installed binary reports %q after the rollback", strings.TrimSpace(got))
	}
	loginWithPassword(t, fx.base, controlPlanePassword)
	if got := s.startCount(); got < 5 {
		t.Fatalf("starts = %d, want the original, three failed and the restored one", got)
	}
}

// A server nothing would restart does not offer to upgrade itself: here the
// installed binary is run by hand, without the supervisor --no-systemd puts in
// front of it.
func TestConsoleUpgradeNeedsAServiceManager(t *testing.T) {
	fx := installControlPlane(t, "v1.2.3")
	stopInstalledControlPlane(t, fx.dir)
	cmd := exec.Command(fx.bin, "serve", "--config", filepath.Join(fx.dir, "server.yaml"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	assertHealthVersion(t, fx.base, "v1.2.3")
	token := loginWithPassword(t, fx.base, controlPlanePassword)
	status, body := consoleUpload(t, fx.base, token, []byte("not even read"))
	if status != http.StatusConflict || !strings.Contains(body, "SERVER_UPGRADE_UNAVAILABLE") || !strings.Contains(body, "no_service_manager") {
		t.Fatalf("upload to an unsupervised server = %d %s", status, body)
	}
}

// A release from before console upgrades never reads the marker, so it would
// neither confirm nor roll back its own start; the console refuses it before
// touching anything and points at the CLI.
func TestConsoleUpgradeRefusesAReleaseFromBeforeIt(t *testing.T) {
	legacy := "#!/bin/sh\ncase \"$1\" in\n  version) echo 'gpuworks v0.9.0 (c0ffee, 2026-01-01T00:00:00Z)' ;;\n" +
		"  *) echo \"unknown subcommand \\\"$1\\\"\" >&2; exit 2 ;;\nesac\n"
	archive := releasePackage(t, "v0.9.0", []byte(legacy))
	fx := installControlPlane(t, "v1.2.3")
	s := supervise(t, fx)
	assertHealthVersion(t, fx.base, "v1.2.3")
	token := loginWithPassword(t, fx.base, controlPlanePassword)

	status, body := consoleUpload(t, fx.base, token, archive)
	if status != http.StatusBadRequest || !strings.Contains(body, "SERVER_UPGRADE_ARCHIVE_INVALID") ||
		!strings.Contains(body, "predates console upgrades") || !strings.Contains(body, "gpuworks upgrade --archive") {
		t.Fatalf("upload of a release from before console upgrades = %d %s", status, body)
	}
	assertHealthVersion(t, fx.base, "v1.2.3")
	if _, err := os.Stat(filepath.Join(fx.dir, "tmp", "upgrade-pending.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused upload left a marker: %v", err)
	}
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, "v1.2.3") {
		t.Fatalf("the installed binary reports %q after a refused upload", strings.TrimSpace(got))
	}
	if got := s.startCount(); got != 1 {
		t.Fatalf("starts = %d; a refused upload must not restart the server", got)
	}
}
