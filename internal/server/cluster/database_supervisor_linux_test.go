//go:build linux && dqlite

package cluster

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
)

// fakeHelper installs a shell script as the database process. Each script
// reproduces one behavior of the real child: its identity line, its exit
// codes and its reaction to signals.
func fakeHelper(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), helperBinaryName)
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helperBinaryEnv, path)
}

func expectHelperDeath(t *testing.T) <-chan error {
	t.Helper()
	died := make(chan error, 1)
	previous := helperDied
	helperDied = func(err error) { died <- err }
	t.Cleanup(func() { helperDied = previous })
	return died
}

const fakeReady = `echo '{"id":7,"address":"127.0.0.1:9001"}'; exec 1>&-; `

func TestHelperReportsIdentityAndStopsOnInterrupt(t *testing.T) {
	died := expectHelperDeath(t)
	fakeHelper(t, `echo "$@" > "$0.args"; `+fakeReady+`exec sleep 60`)
	process, info, err := startHelperUntilReady(t.Context(), []string{"--dir", "/d", "--address", "127.0.0.1:9001"})
	if err != nil {
		t.Fatal(err)
	}
	if info != (helperIdentity{ID: 7, Address: "127.0.0.1:9001"}) {
		t.Fatalf("identity=%+v", info)
	}
	args, err := os.ReadFile(os.Getenv(helperBinaryEnv) + ".args")
	if err != nil || strings.TrimSpace(string(args)) != "--dir /d --address 127.0.0.1:9001" {
		t.Fatalf("args=%q err=%v", args, err)
	}
	if process.cmd.SysProcAttr == nil || process.cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatal("child would outlive a killed server")
	}
	process.guard()
	started := time.Now()
	if err := process.stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("interrupt was not delivered")
	}
	if err := process.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop returned before the child exited: %v", err)
	}
	select {
	case err := <-died:
		t.Fatalf("requested stop was reported as a death: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHelperStopKillsChildThatIgnoresInterrupt(t *testing.T) {
	fakeHelper(t, `trap '' INT; `+fakeReady+`exec sleep 60`)
	process, _, err := startHelperUntilReady(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = process.stop(300 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "was killed") {
		t.Fatalf("err=%v", err)
	}
	if waited := time.Since(started); waited < 300*time.Millisecond || waited > 3*time.Second {
		t.Fatalf("waited %s", waited)
	}
	if err := process.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("child survived: %v", err)
	}
}

func TestHelperStartFailures(t *testing.T) {
	for name, test := range map[string]struct{ script, want string }{
		"malformed line":   {`echo 'listening on 9001'; exec sleep 60`, "did not report its address"},
		"unknown field":    {`echo '{"id":7,"address":"127.0.0.1:9001","pid":1}'; exec sleep 60`, "did not report its address"},
		"missing identity": {`echo '{"id":0,"address":""}'; exec sleep 60`, "incomplete identity"},
		"early exit":       {`echo 'raft directory is damaged' >&2; exit 3`, "raft directory is damaged"},
		"silent exit":      {`exit 1`, "did not report its address"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeHelper(t, test.script)
			started := time.Now()
			process, _, err := startHelperUntilReady(t.Context(), nil)
			if err == nil {
				_ = process.stop(0)
				t.Fatal("start succeeded")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v", err)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatal("a failed child was not reaped promptly")
			}
		})
	}
}

func TestHelperStartCapsStderrInTheError(t *testing.T) {
	fakeHelper(t, `printf 'first-line\n' >&2; head -c 100000 /dev/zero | tr '\0' x >&2; printf 'last-line\n' >&2; exit 1`)
	_, _, err := startHelperUntilReady(t.Context(), nil)
	if err == nil {
		t.Fatal("start succeeded")
	}
	text := err.Error()
	if !strings.Contains(text, "first-line") || strings.Contains(text, "last-line") {
		t.Fatalf("error does not keep the start of stderr: %.80q", text)
	}
	if len(text) > helperStderrLimit+200 {
		t.Fatalf("error carries %d bytes of stderr", len(text))
	}
}

func TestHelperStartRetriesWhileThePreviousChildHoldsTheLock(t *testing.T) {
	fakeHelper(t, `n=$(cat "$0.n" 2>/dev/null || echo 0); echo $((n+1)) > "$0.n"
if [ "$n" -lt 2 ]; then echo "lock /d.helper.lock: file lock is held by another process" >&2; exit 1; fi
`+fakeReady+`exec sleep 60`)
	process, _, err := startHelperUntilReady(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer process.stop(0)
	attempts, _ := os.ReadFile(os.Getenv(helperBinaryEnv) + ".n")
	if strings.TrimSpace(string(attempts)) != "3" {
		t.Fatalf("attempts=%q", attempts)
	}
}

func TestHelperStartDoesNotRetryOtherFailuresOrOutliveItsContext(t *testing.T) {
	fakeHelper(t, `n=$(cat "$0.n" 2>/dev/null || echo 0); echo $((n+1)) > "$0.n"; echo 'no such directory' >&2; exit 1`)
	if _, _, err := startHelperUntilReady(t.Context(), nil); err == nil {
		t.Fatal("start succeeded")
	}
	if attempts, _ := os.ReadFile(os.Getenv(helperBinaryEnv) + ".n"); strings.TrimSpace(string(attempts)) != "1" {
		t.Fatalf("attempts=%q", attempts)
	}
	fakeHelper(t, `echo $$ > "$0.pid"; exec sleep 60`)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := startHelperUntilReady(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	raw, err := os.ReadFile(os.Getenv(helperBinaryEnv) + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, c := range strings.TrimSpace(string(raw)) {
		pid = pid*10 + int(c-'0')
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("silent child %d was left running: %v", pid, err)
	}
}

func TestHelperDeathIsReportedOnlyWhileGuarded(t *testing.T) {
	died := expectHelperDeath(t)
	fakeHelper(t, fakeReady+`sleep 0.2; exit 7`)
	process, _, err := startHelperUntilReady(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	process.guard()
	select {
	case err := <-died:
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("death=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unexpected exit was not reported")
	}

	// A child that dies before the database is handed over belongs to the
	// caller of the failed open.
	process, _, err = startHelperUntilReady(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	<-process.exited
	select {
	case err := <-died:
		t.Fatalf("unguarded exit was reported: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// Guarding afterwards must still notice that the child is gone.
	process.guard()
	select {
	case <-died:
	case <-time.After(time.Second):
		t.Fatal("guard missed an exit that happened before it")
	}
}

func TestHelperBinaryMustExist(t *testing.T) {
	t.Setenv(helperBinaryEnv, filepath.Join(t.TempDir(), "absent"))
	if _, _, err := startHelperUntilReady(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "start dqlite helper") {
		t.Fatalf("err=%v", err)
	}
	if _, err := runHelper(t.Context(), "id", "--address", "127.0.0.1:9001"); err == nil {
		t.Fatal("offline command ran without a binary")
	}
	// Without the override the binary is looked up beside this executable.
	t.Setenv(helperBinaryEnv, "")
	if _, err := helperBinary(); err == nil || !strings.Contains(err.Error(), "not installed beside the server executable") {
		t.Fatalf("err=%v", err)
	}
	cfg := DatabaseConfig{Dir: filepath.Join(t.TempDir(), "raft"), Address: "127.0.0.1:9001", Name: "gpuworks.db"}
	if _, err := StartDatabase(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "not installed beside the server executable") {
		t.Fatalf("err=%v", err)
	}
}

func TestStartDatabaseRejectsChildOnAnotherAddress(t *testing.T) {
	fakeHelper(t, `echo $$ > "$0.pid"; echo '{"id":7,"address":"127.0.0.1:9777"}'; exec 1>&-; exec sleep 60`)
	cfg := DatabaseConfig{Dir: filepath.Join(t.TempDir(), "raft"), Address: "127.0.0.1:9001", Name: "gpuworks.db"}
	_, err := StartDatabase(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), `stored database address "127.0.0.1:9777" differs`) {
		t.Fatalf("err=%v", err)
	}
	raw, _ := os.ReadFile(os.Getenv(helperBinaryEnv) + ".pid")
	var pid int
	for _, c := range strings.TrimSpace(string(raw)) {
		pid = pid*10 + int(c-'0')
	}
	if pid == 0 {
		t.Fatal("child did not start")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("rejected child %d was left running: %v", pid, err)
	}
}

func TestHelperTLSFilesArePrivateAndRequired(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "raft")
	if _, err := writeHelperTLS(DatabaseConfig{Dir: dir, TLSCertPEM: []byte("c"), TLSKeyPEM: []byte("k")}); err == nil {
		t.Fatal("missing authority was accepted")
	}
	cfg := DatabaseConfig{Dir: dir, TLSCertPEM: []byte("cert"), TLSKeyPEM: []byte("key"), TLSCACertPEM: []byte("ca")}
	out, err := writeHelperTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out != dir+".helper-tls" {
		t.Fatalf("dir=%s", out)
	}
	// A rewrite replaces a widened file and does not write through a link.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(out, "cert.pem"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(out, "key.pem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(out, "key.pem")); err != nil {
		t.Fatal(err)
	}
	cfg.TLSKeyPEM = []byte("key2")
	if _, err := writeHelperTLS(cfg); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"cert.pem": "cert", "key.pem": "key2", "ca.pem": "ca"} {
		info, err := os.Lstat(filepath.Join(out, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
		if body, _ := os.ReadFile(filepath.Join(out, name)); string(body) != want {
			t.Fatalf("%s=%q", name, body)
		}
	}
	if body, _ := os.ReadFile(victim); string(body) != "keep" {
		t.Fatal("key was written through a symbolic link")
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode: %v %v", info, err)
	}
}

func TestOfflineCommandsCarryArgumentsAndErrors(t *testing.T) {
	fakeHelper(t, `echo "$@" >> "$0.args"
case "$1" in
id) echo 4242 ;;
inspect) echo '{"schema":"abc"}' ;;
describe-last) echo 'raft segment is corrupt' >&2; exit 1 ;;
reconfigure) exit 0 ;;
esac`)
	ctx := t.Context()
	if id, err := generateMemberID(ctx, "10.0.0.1:9001"); err != nil || id != 4242 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	info := client.NodeInfo{ID: 9, Address: "10.0.0.1:9001", Role: client.Voter}
	if schema, err := inspectIsolatedConversionSource(ctx, "/d", "gpuworks.db", info); err != nil || schema != "abc" {
		t.Fatalf("schema=%q err=%v", schema, err)
	}
	if err := reconfigureSingleVoter(ctx, "/d", info); err != nil {
		t.Fatal(err)
	}
	if err := describeLastRaftEntry(ctx, "/d"); err == nil || err.Error() != "raft segment is corrupt" {
		t.Fatalf("err=%v", err)
	}
	args, _ := os.ReadFile(os.Getenv(helperBinaryEnv) + ".args")
	want := "id --address 10.0.0.1:9001\ninspect --dir /d --id 9 --address 10.0.0.1:9001 --name gpuworks.db\nreconfigure --dir /d --id 9 --address 10.0.0.1:9001 --role voter\ndescribe-last --dir /d\n"
	if string(args) != want {
		t.Fatalf("args:\n%s", args)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := generateMemberID(canceled, "10.0.0.1:9001"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	fakeHelper(t, `echo not-a-number`)
	if _, err := generateMemberID(ctx, "10.0.0.1:9001"); err == nil {
		t.Fatal("malformed member id was accepted")
	}
	fakeHelper(t, `echo '{"schema":""}'`)
	if _, err := inspectIsolatedConversionSource(ctx, "/d", "gpuworks.db", info); err == nil {
		t.Fatal("empty schema was accepted")
	}
}

// churnThreads ends many OS threads: the runtime terminates a thread whose
// locked goroutine returns.
func churnThreads(n int) {
	for i := 0; i < n; i++ {
		done := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			close(done)
		}()
		<-done
	}
}

func processAlive(pid int) bool {
	// A reaped or never-started pid reports ESRCH; a zombie still answers, so
	// read its state as well.
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func waitProcessGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !processAlive(pid)
}

func TestHelperSurvivesThreadChurnOfAHealthyServer(t *testing.T) {
	// Control: the kernel really does kill a child when the thread that
	// forked it exits, even though this process keeps running.
	control := exec.Command("sleep", "60")
	control.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		started <- control.Start()
	}()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	go func() { _ = control.Wait() }()
	if !waitProcessGone(control.Process.Pid, 5*time.Second) {
		_ = control.Process.Kill()
		t.Fatal("control child outlived its forking thread; this test cannot prove anything on this kernel")
	}

	died := expectHelperDeath(t)
	fakeHelper(t, fakeReady+`exec sleep 60`)
	process, _, err := startHelperUntilReady(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	process.guard()
	pid := process.cmd.Process.Pid
	for round := 0; round < 20; round++ {
		churnThreads(50)
		runtime.GC()
		if !processAlive(pid) {
			t.Fatalf("child died after thread churn round %d", round)
		}
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-died:
		t.Fatalf("child was killed under a healthy parent: %v", err)
	default:
	}
	if !processAlive(pid) {
		t.Fatal("child died under a healthy parent")
	}
	if err := process.stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

const helperParentEnv = "GPUWORKS_TEST_HELPER_PARENT"

// TestHelperParentProcess is the re-executed parent of the test below. It
// starts the child the way a server does, reports the child's pid and waits to
// be killed.
func TestHelperParentProcess(t *testing.T) {
	if os.Getenv(helperParentEnv) != "1" {
		t.Skip("runs only as the re-executed parent of TestHelperDiesWithAKilledServer")
	}
	process, _, err := startHelperUntilReady(context.Background(), nil)
	if err != nil {
		fmt.Println("parent-error", err)
		os.Exit(3)
	}
	process.guard()
	churnThreads(200)
	fmt.Println("child-pid", process.cmd.Process.Pid)
	select {}
}

func TestHelperDiesWithAKilledServer(t *testing.T) {
	fakeHelper(t, fakeReady+`exec sleep 60`)
	parent := exec.Command(os.Args[0], "-test.run=^TestHelperParentProcess$", "-test.v")
	parent.Env = append(os.Environ(), helperParentEnv+"=1")
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	pid := 0
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if text, ok := strings.CutPrefix(scanner.Text(), "child-pid "); ok {
			pid, _ = strconv.Atoi(text)
			break
		}
		if strings.HasPrefix(scanner.Text(), "parent-error") {
			t.Fatal(scanner.Text())
		}
	}
	if pid == 0 {
		t.Fatal("parent did not report its child")
	}
	// The child survived the parent's own thread churn and is still running.
	time.Sleep(200 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatal("child died while its parent was healthy")
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if !waitProcessGone(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("child %d outlived a killed parent", pid)
	}
}
