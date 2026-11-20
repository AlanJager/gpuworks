//go:build linux && cgo && dqlite

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	dqlite "github.com/canonical/go-dqlite/v3"
	"github.com/canonical/go-dqlite/v3/client"

	"gpuworks.ai/gpuworks/internal/filelock"
)

// The test binary doubles as the helper so process-level behaviour (standard
// output closing, signals, exit codes) is observed exactly as the server sees it.
func TestMain(m *testing.M) {
	if os.Getenv("GPUWORKS_DQLITE_TEST_HELPER") == "1" {
		os.Exit(run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type result struct {
	code   int
	stdout string
	stderr string
}

func helperCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GPUWORKS_DQLITE_TEST_HELPER=1")
	return cmd
}

func runHelper(t *testing.T, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := helperCommand(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("helper did not run: %v", err)
		}
		code = exit.ExitCode()
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// dataDir returns an existing private directory: the server creates it before
// it starts the helper.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "db")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type served struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	line   string
	id     uint64
	addr   string
}

// startAnnounced starts a long-running mode, consumes its single announcement
// line and requires standard output to reach end of file as the contract states.
func startAnnounced(t *testing.T, cmd *exec.Cmd) *served {
	t.Helper()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	reader := bufio.NewReader(pipe)
	type line struct {
		text string
		err  error
	}
	lines := make(chan line, 1)
	go func() {
		text, err := reader.ReadString('\n')
		lines <- line{text, err}
	}()
	var first line
	select {
	case first = <-lines:
	case <-time.After(60 * time.Second):
		t.Fatalf("helper printed no announcement; stderr: %s", stderr)
	}
	if first.err != nil {
		t.Fatalf("announcement: %v; stderr: %s", first.err, stderr)
	}
	rest, err := io.ReadAll(reader)
	if err != nil || len(rest) != 0 {
		t.Fatalf("standard output after the announcement: %q, %v", rest, err)
	}
	return &served{cmd: cmd, stderr: stderr, line: first.text}
}

func startServe(t *testing.T, args ...string) *served {
	t.Helper()
	node := startAnnounced(t, helperCommand(context.Background(), args...))
	var announced struct {
		ID      uint64 `json:"id"`
		Address string `json:"address"`
	}
	decoder := json.NewDecoder(strings.NewReader(node.line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&announced); err != nil {
		t.Fatalf("announcement %q: %v", node.line, err)
	}
	node.id, node.addr = announced.ID, announced.Address
	return node
}

func (s *served) stop(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper exit after %v: %v; stderr: %s", sig, err, s.stderr)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("helper ignored %v; stderr: %s", sig, s.stderr)
	}
}

func TestIDCommandPrintsOneUsableMemberID(t *testing.T) {
	got := runHelper(t, "id", "--address", "10.1.2.3:9001")
	if got.code != 0 || got.stderr != "" {
		t.Fatalf("id: %+v", got)
	}
	if !strings.HasSuffix(got.stdout, "\n") || strings.Count(got.stdout, "\n") != 1 {
		t.Fatalf("id output %q is not one line", got.stdout)
	}
	id, err := strconv.ParseUint(strings.TrimSpace(got.stdout), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	// GenerateID mixes the address with the current time, so only the
	// reserved ids are predictable.
	if id == 0 || id == 1 || id == dqlite.BootstrapID {
		t.Fatalf("id command returned reserved id %d", id)
	}
}

func TestUsageErrorsExitTwoWithoutOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	cases := map[string][]string{
		"unknown command":            {"bogus"},
		"serve without dir":          {"--address", "127.0.0.1:1"},
		"serve without address":      {"--dir", dir},
		"serve unknown flag":         {"--dir", dir, "--address", "127.0.0.1:1", "--nope"},
		"serve partial tls":          {"--dir", dir, "--address", "127.0.0.1:1", "--cert", "c.pem"},
		"serve tls no policy":        {"--dir", dir, "--address", "127.0.0.1:1", "--cert", "c.pem", "--key", "k.pem", "--ca", "ca.pem"},
		"serve policy only":          {"--dir", dir, "--address", "127.0.0.1:1", "--policy", "p.json"},
		"serve untiered latency":     {"--dir", dir, "--address", "127.0.0.1:1", "--network-latency-ms", "100"},
		"serve zero latency":         {"--dir", dir, "--address", "127.0.0.1:1", "--network-latency-ms", "0"},
		"serve latency not a number": {"--dir", dir, "--address", "127.0.0.1:1", "--network-latency-ms", "fast"},
		"reconfigure both":           {"reconfigure", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1", "--member", "3,127.0.0.1:2,voter"},
		"reconfigure role mix":       {"reconfigure", "--dir", dir, "--role", "voter", "--member", "3,127.0.0.1:2,voter"},
		"reconfigure bad entry":      {"reconfigure", "--dir", dir, "--member", "3,127.0.0.1:2"},
		"reconfigure dup id":         {"reconfigure", "--dir", dir, "--member", "3,127.0.0.1:2,voter", "--member", "3,127.0.0.1:3,voter"},
		"reconfigure zero id":        {"reconfigure", "--dir", dir, "--member", "0,127.0.0.1:2,voter"},
		"id without address":         {"id"},
		"describe without dir":       {"describe-last"},
		"reconfigure no id":          {"reconfigure", "--dir", dir, "--address", "127.0.0.1:1"},
		"reconfigure bad role":       {"reconfigure", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1", "--role", "leader"},
		"isolated without id":        {"isolated", "--dir", dir, "--address", "127.0.0.1:1"},
		"inspect without name":       {"inspect", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			got := runHelper(t, args...)
			if got.code != 2 || got.stdout != "" || got.stderr == "" {
				t.Fatalf("%v: %+v", args, got)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("usage error touched the data directory: %v", err)
			}
		})
	}
}

func TestParseRole(t *testing.T) {
	for text, want := range map[string]client.NodeRole{"voter": client.Voter, "stand-by": client.StandBy, "spare": client.Spare} {
		got, err := parseRole(text)
		if err != nil || got != want {
			t.Fatalf("parseRole(%q) = %v, %v", text, got, err)
		}
	}
	if _, err := parseRole("Voter"); err == nil {
		t.Fatal("role names are case-sensitive")
	}
}

func TestServeAnnouncesThenStopsOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := dataDir(t)
			address := freeAddress(t)
			node := startServe(t, "--dir", dir, "--address", address)
			if node.addr != address || node.id == 0 {
				t.Fatalf("announcement id=%d address=%q, want address %q", node.id, node.addr, address)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := client.New(ctx, address)
			if err != nil {
				t.Fatal(err)
			}
			members, err := c.Cluster(ctx)
			_ = c.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(members) != 1 || members[0].ID != node.id || members[0].Address != address || members[0].Role != client.Voter {
				t.Fatalf("members = %+v", members)
			}
			node.stop(t, sig)
			// The lock must be free again and the same directory must restart
			// with the same identity.
			again := startServe(t, "--dir", dir, "--address", address)
			if again.id != node.id {
				t.Fatalf("restart changed the member id from %d to %d", node.id, again.id)
			}
			again.stop(t, syscall.SIGTERM)
		})
	}
}

// Both tiers' latencies start a node; the directory keeps its identity when
// the tier changes between two starts, as it does on a seed that records its
// tier after its first single-member start.
func TestServeAcceptsEachStorageTiersLatency(t *testing.T) {
	dir := dataDir(t)
	address := freeAddress(t)
	first := startServe(t, "--dir", dir, "--address", address, "--network-latency-ms", "10")
	first.stop(t, syscall.SIGTERM)
	second := startServe(t, "--dir", dir, "--address", address, "--network-latency-ms", "50")
	if second.id != first.id || second.addr != address {
		t.Fatalf("restart on another tier: id %d address %q, want id %d address %q", second.id, second.addr, first.id, address)
	}
	second.stop(t, syscall.SIGTERM)
}

func TestSecondHelperOnTheSameDirectoryIsRefused(t *testing.T) {
	dir := dataDir(t)
	node := startServe(t, "--dir", dir, "--address", freeAddress(t))
	got := runHelper(t, "--dir", dir, "--address", freeAddress(t))
	if got.code != 1 || got.stdout != "" || got.stderr == "" {
		t.Fatalf("second serve: %+v", got)
	}
	got = runHelper(t, "describe-last", "--dir", dir)
	if got.code != 1 || got.stdout != "" {
		t.Fatalf("describe-last under a running node: %+v", got)
	}
	node.stop(t, syscall.SIGTERM)
}

func TestHeldLockRefusesEveryDirectoryMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	release, err := filelock.Acquire(dir + ".helper.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, args := range [][]string{
		{"--dir", dir, "--address", "127.0.0.1:1"},
		{"describe-last", "--dir", dir},
		{"reconfigure", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1"},
		{"isolated", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1"},
		{"inspect", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1", "--name", "gpuworks"},
	} {
		got := runHelper(t, args...)
		if got.code != 1 || got.stdout != "" || got.stderr == "" {
			t.Fatalf("%v: %+v", args, got)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refused helper created the data directory: %v", err)
	}
}

func TestMissingDataDirectoryFailsAndIsNotCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	for _, args := range [][]string{
		{"--dir", dir, "--address", freeAddress(t)},
		{"reconfigure", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1"},
		{"isolated", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1"},
		{"inspect", "--dir", dir, "--id", "2", "--address", "127.0.0.1:1", "--name", "gpuworks"},
	} {
		got := runHelper(t, args...)
		if got.code != 1 || got.stdout != "" || got.stderr == "" {
			t.Fatalf("%v: %+v", args, got)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%v created the data directory: %v", args, err)
		}
	}
}

func TestUnreachableSeedsStillAnnounceAndStop(t *testing.T) {
	dir := dataDir(t)
	address := freeAddress(t)
	// Joining happens after the node is listening. The server decides how
	// long to wait for membership, so the helper must announce and stay
	// stoppable while every seed refuses connections.
	node := startServe(t, "--dir", dir, "--address", address, "--seeds", freeAddress(t)+","+freeAddress(t))
	if node.addr != address {
		t.Fatalf("announced %q, want %q", node.addr, address)
	}
	node.stop(t, syscall.SIGTERM)
}
