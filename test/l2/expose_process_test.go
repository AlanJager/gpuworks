//go:build multiproc

package l2

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	exposelib "gpuworks.ai/gpuworks/internal/server/expose"
)

// The public entry point is hosted by a connector service of its own
// (`gpuworks expose run`), which `expose up` starts in the background, apart
// from the server: service → connector-host helper → cloudflared. Its process
// lifecycle is a multiprocess property — the connector outlives the server, and
// the helper's liveness pipe still takes the connector down with the service.
//
// Readiness is not asserted here: a fake connector cannot pass the public HTTPS
// probe, and the plan keeps that judgement in the L1 harness (with an injectable
// checker) plus a manual online run. What this case proves is what only real
// processes can: the chain, the server restarting underneath, no leftovers
// after the service is killed or closed, log rotation and a single service.

// exposeprocConnectorScript behaves like a quick tunnel and records every start,
// so a restart is observable by pid.
const exposeprocConnectorScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo 'cloudflared version 2026.9.1 (fake)'
	exit 0
fi
echo "$$" >> "$FAKE_CF_STARTS"
echo "$$" > "$FAKE_CF_PIDFILE"
echo 'INF |  Your quick Tunnel has been created! Visit it at:  |'
echo "INF |  https://l2-$$.trycloudflare.com  |"
echo 'INF Registered tunnel connection connIndex=0 event=0 ip=198.41.192.1 location=lax99 protocol=quic'
if [ -n "$FAKE_CF_BULK" ]; then
	# Enough output to make the server's rotating writer rotate for real.
	head -c 12000000 /dev/zero | tr '\0' 'a'
	echo
fi
while true; do sleep 1; done
`

type exposeprocHarness struct {
	bin       string
	dataDir   string
	binDir    string
	events    string
	pidFile   string
	starts    string
	port      string
	server    *exec.Cmd
	serverErr *os.File
}

func newExposeprocHarness(t *testing.T, bulk bool) *exposeprocHarness {
	t.Helper()
	h := &exposeprocHarness{
		bin:     buildBinary(t, "./cmd/gpuworks"),
		dataDir: t.TempDir(),
		binDir:  t.TempDir(),
		port:    freePort(t),
	}
	h.events = filepath.Join(h.binDir, "events.log")
	h.pidFile = filepath.Join(h.binDir, "connector.pid")
	h.starts = filepath.Join(h.binDir, "starts")
	script := exposeprocConnectorScript
	if err := os.WriteFile(filepath.Join(h.binDir, "cloudflared"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if bulk {
		t.Setenv("FAKE_CF_BULK", "1")
	}
	t.Cleanup(func() {
		if h.server != nil && h.server.Process != nil {
			_ = h.server.Process.Kill()
			_, _ = h.server.Process.Wait()
		}
		if h.serverErr != nil {
			_ = h.serverErr.Close()
		}
		// Anything the case started must be gone; kill by recorded pid so a
		// failure does not leave a connector serving this machine. The
		// service goes first, or it would start a replacement.
		for _, pid := range h.servicePIDs() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		for _, pid := range h.connectorPIDs() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return h
}

// start launches the server with the fake connector on its PATH.
func (h *exposeprocHarness) start(t *testing.T) {
	t.Helper()
	h.server = serverCommand(t, "multiproc-password", h.bin, "serve", "--addr", "127.0.0.1:"+h.port, "--data", h.dataDir)
	h.server.Env = h.exposeEnv()
	errFile, err := os.Create(h.events)
	if err != nil {
		t.Fatal(err)
	}
	h.serverErr = errFile
	h.server.Stdout, h.server.Stderr = errFile, errFile
	if err := h.server.Start(); err != nil {
		t.Fatal(err)
	}
	waitHTTP(t, "http://127.0.0.1:"+h.port+"/healthz", 30*time.Second, func(resp *http.Response) bool { return resp.StatusCode == 200 })
}

func (h *exposeprocHarness) stop(t *testing.T, signal os.Signal) {
	t.Helper()
	if h.server == nil || h.server.Process == nil {
		return
	}
	_ = h.server.Process.Signal(signal)
	_, _ = h.server.Process.Wait()
	h.server = nil
}

// exposeEnv is the environment of every gpuworks process in these cases: the
// fake connector on PATH, and where it records itself.
func (h *exposeprocHarness) exposeEnv() []string {
	return append(os.Environ(),
		"PATH="+h.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_CF_PIDFILE="+h.pidFile,
		"FAKE_CF_STARTS="+h.starts,
	)
}

// up runs `gpuworks expose up` the way an operator does. It waits for a ready
// address, which a fake connector never gets, so it is interrupted once the
// connector is running — the request stays with the service.
func (h *exposeprocHarness) up(t *testing.T) {
	t.Helper()
	waitFile(t, exposelib.NewStore(h.dataDir).ServerOriginPath(), 30*time.Second)
	cmd := exec.Command(h.bin, "expose", "up", "--data", h.dataDir)
	cmd.Env = h.exposeEnv()
	out, err := os.Create(filepath.Join(h.binDir, "up.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.waitConnectors(t, len(h.connectorPIDs())+1, 60*time.Second)
	_ = cmd.Process.Signal(syscall.SIGINT)
	_ = cmd.Wait()
}

// cli runs a short gpuworks expose subcommand and returns its output.
func (h *exposeprocHarness) cli(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(h.bin, append([]string{"expose"}, append(args, "--data", h.dataDir)...)...)
	cmd.Env = h.exposeEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// servicePIDs is every connector service running for this data dir.
func (h *exposeprocHarness) servicePIDs() []int {
	out, _ := exec.Command("pgrep", "-f", "expose run --data "+h.dataDir).Output()
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}

func (h *exposeprocHarness) status() *exposelib.Status {
	st, _ := exposelib.NewStore(h.dataDir).ReadStatus()
	return st
}

// connectorPIDs is every connector the fake has started and that is still
// running. A zombie counts as gone: after SIGKILL the process table keeps the
// pid until the helper reaps it, and kill(pid, 0) succeeds for that window —
// treating it as running would read as "the connector was never replaced".
func (h *exposeprocHarness) connectorPIDs() []int {
	raw, err := os.ReadFile(h.starts)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 0 {
			continue
		}
		if syscall.Kill(pid, 0) != nil {
			continue
		}
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

func (h *exposeprocHarness) waitConnectors(t *testing.T, want int, timeout time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []int
	for time.Now().Before(deadline) {
		last = h.connectorPIDs()
		if len(last) == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	raw, _ := os.ReadFile(h.starts)
	statusRaw, _ := os.ReadFile(exposelib.NewStore(h.dataDir).StatusPath())
	logRaw, _ := os.ReadFile(h.events)
	t.Fatalf("connectors = %v, want %d\nstarts: %s\nstatus: %s\nserver log tail:\n%s",
		last, want, raw, statusRaw, tailLines(string(logRaw), 25))
	return nil
}

// waitConnectorGone waits until the connector with this pid is no longer
// running (a zombie counts as gone, see connectorPIDs).
func (h *exposeprocHarness) waitConnectorGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !slices.Contains(h.connectorPIDs(), pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("connector pid %d is still running %s after SIGKILL", pid, timeout)
}

// tailLines keeps the end of a log, which is where a failure explains itself.
func tailLines(raw string, n int) string {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// procInfo returns a process's parent pid and its own command line.
func (h *exposeprocHarness) procInfo(t *testing.T, pid int) (int, string) {
	t.Helper()
	out, err := exec.Command("ps", "-o", "ppid=,args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps for pid %d: %v", pid, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		t.Fatalf("unexpected ps output for pid %d: %q", pid, out)
	}
	ppid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("ppid %q: %v", fields[0], err)
	}
	return ppid, strings.Join(fields[1:], " ")
}

func TestProcessExposeConnectorOutlivesTheServer(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("ps required in strict mode: %v", err)
		}
		t.Skipf("ps unavailable: %v", err)
	}
	h := newExposeprocHarness(t, false)
	h.start(t)
	h.up(t)

	// service → helper → connector, and the server is nowhere in that chain.
	first := h.waitConnectors(t, 1, 60*time.Second)[0]
	helperPID, connectorArgs := h.procInfo(t, first)
	if !strings.Contains(connectorArgs, "cloudflared") {
		t.Fatalf("pid %d is not the connector: %s", first, connectorArgs)
	}
	servicePID, helperArgs := h.procInfo(t, helperPID)
	if !strings.Contains(helperArgs, "connector-host") || !strings.Contains(helperArgs, h.bin) {
		t.Fatalf("the connector's parent (%d: %s) should be this binary's connector-host helper", helperPID, helperArgs)
	}
	_, serviceArgs := h.procInfo(t, servicePID)
	if !strings.Contains(serviceArgs, "expose run") || servicePID == h.server.Process.Pid {
		t.Fatalf("the helper's parent (%d: %s) should be the connector service, not the server (%d)", servicePID, serviceArgs, h.server.Process.Pid)
	}
	if out := h.cli(t, "status"); !strings.Contains(out, "background process") {
		t.Fatalf("status does not say what runs the connector:\n%s", out)
	}

	// The server dies and comes back; the connector, and so the address, stay.
	_ = h.server.Process.Kill()
	_, _ = h.server.Process.Wait()
	h.server = nil
	time.Sleep(time.Second)
	if now := h.connectorPIDs(); len(now) != 1 || now[0] != first {
		t.Fatalf("connectors after the server was killed = %v, want the original %d", now, first)
	}
	h.start(t)
	time.Sleep(time.Second)
	if now := h.connectorPIDs(); len(now) != 1 || now[0] != first {
		t.Fatalf("connectors after the server restarted = %v, want the original %d", now, first)
	}

	// A connector that dies is replaced by the service.
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitConnectorGone(t, first, 30*time.Second)
	second := h.waitConnectors(t, 1, 60*time.Second)[0]

	// SIGKILL of the service: nothing can prepare for it, and the liveness
	// pipe still takes the connector down.
	if err := syscall.Kill(servicePID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitConnectorGone(t, second, 30*time.Second)

	// up starts a service again; down closes the entry point and the service
	// exits with it.
	h.up(t)
	h.waitConnectors(t, 1, 60*time.Second)
	if out := h.cli(t, "down"); !strings.Contains(out, "closed") {
		t.Fatalf("down did not report the entry point closed:\n%s", out)
	}
	h.waitConnectors(t, 0, 30*time.Second)
	if pids := h.servicePIDs(); len(pids) != 0 {
		t.Fatalf("connector services still running after down: %v", pids)
	}
}

func TestProcessExposeSingleServiceAndLogRotation(t *testing.T) {
	h := newExposeprocHarness(t, true)
	h.start(t)
	h.up(t)

	// The connector writes more than the rotating writer's limit; the service
	// rotates it, which only the process that owns the pipe can do.
	logPath := exposelib.NewStore(h.dataDir).LogPath()
	waitFile(t, logPath+".1", 60*time.Second)

	// A second service for the same data dir refuses to start.
	second := exec.Command(h.bin, "expose", "run", "--data", h.dataDir)
	second.Env = h.exposeEnv()
	out, err := second.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "already running") {
		t.Fatalf("a second connector service should refuse to start: %v\n%s", err, out)
	}
	if got := len(h.connectorPIDs()); got != 1 {
		t.Fatalf("connectors = %d, want exactly one", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}
