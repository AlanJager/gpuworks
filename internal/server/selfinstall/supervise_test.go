//go:build linux || darwin

package selfinstall

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The child is a shell script standing in for `serve`: it records each start
// with the variable the supervisor promised, exits twice, and on the third
// start runs until it is told to stop, recording the signal it got.
func TestSuperviseStartsTheServerAgainAndStopsIt(t *testing.T) {
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	script := filepath.Join(dir, "gpuworks")
	// The trap is set before the start is recorded: the test stops the
	// supervisor as soon as it sees the third start.
	writeExecutable(t, script, `#!/bin/sh
trap 'echo "term" >> `+starts+`; exit 0' TERM
echo "start $GPUWORKS_SUPERVISED $*" >> `+starts+`
n=$(grep -c '^start' `+starts+`)
if [ "$n" -lt 3 ]; then exit 1; fi
while :; do sleep 0.05; done
`)
	pidFile := filepath.Join(dir, "gpuworks.pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Supervise(ctx, SuperviseOptions{
			Binary: script, Args: []string{"serve", "--config", "server.yaml"},
			PIDFile: pidFile, Delay: 10 * time.Millisecond,
		})
	}()

	waitFor(t, func() bool { return strings.Count(readFileOr(starts), "start") == 3 })
	if got := strings.TrimSpace(readFileOr(pidFile)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file = %q, want the supervisor's pid %d", got, os.Getpid())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not return after its context ended")
	}
	lines := strings.Split(strings.TrimSpace(readFileOr(starts)), "\n")
	want := []string{"start 1 serve --config server.yaml", "start 1 serve --config server.yaml", "start 1 serve --config server.yaml", "term"}
	if !slices.Equal(lines, want) {
		t.Fatalf("child record = %q, want %q", lines, want)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("the supervisor left its pid file: %v", err)
	}
}

// A binary that cannot be started is tried again rather than given up on: a
// console upgrade renames the new file over the path, so the next try finds it.
func TestSuperviseRetriesABinaryThatCannotStart(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gpuworks")
	marker := filepath.Join(dir, "started")
	var logged []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Supervise(ctx, SuperviseOptions{Binary: script, Delay: 10 * time.Millisecond,
			Logf: func(format string, args ...any) { logged = append(logged, format) }})
	}()
	time.Sleep(50 * time.Millisecond)
	writeExecutable(t, script, "#!/bin/sh\ntouch "+marker+"\nexec sleep 30\n")
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("supervise: %v", err)
	}
	if len(logged) == 0 || !strings.HasPrefix(logged[0], "supervise: start ") {
		t.Fatalf("the failed start was not reported: %q", logged)
	}
}

func TestWithoutSuperviseFlag(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"--supervise", "--config", "a.yaml"}, []string{"--config", "a.yaml"}},
		{[]string{"--config", "a.yaml", "-supervise"}, []string{"--config", "a.yaml"}},
		{[]string{"--supervise=true", "--addr", ":1"}, []string{"--addr", ":1"}},
		{[]string{"--addr", ":1", "--", "--supervise"}, []string{"--addr", ":1", "--", "--supervise"}},
		{[]string{"--supervisor-x"}, []string{"--supervisor-x"}},
	} {
		if got := WithoutSuperviseFlag(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("WithoutSuperviseFlag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTakeSupervisedClearsTheVariable(t *testing.T) {
	t.Setenv(SupervisedEnv, "1")
	if !TakeSupervised() {
		t.Fatal("a supervised start was not recognised")
	}
	if _, set := os.LookupEnv(SupervisedEnv); set {
		t.Fatal("the variable would reach the processes the server starts")
	}
	if TakeSupervised() {
		t.Fatal("a second read still reports supervised")
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func readFileOr(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
