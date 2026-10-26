//go:build linux

package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Captured from an owned /bin/true while its parent reaped it. Linux initializes
// PGID/SID to -1 when do_task_stat cannot lock the exiting task's sighand.
const exitingTaskStat = "1094317 (true) X 0 -1 -1 0 -1 4227084 57 0 0 0 0 0 0 0 20 0 0 0 83399945 0 0 0 0 0 0 0 0 0 0 0 0 1 0 0 17 27 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"

func TestTerminalStatDistinguishesExitingTaskFromInvalidIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, stat string
		status     int
	}{
		{"dead", exitingTaskStat, 75},
		// State is sampled before lock_task_sighand, so it can precede the exit.
		{"running_snapshot", strings.Replace(exitingTaskStat, ") X ", ") R ", 1), 75},
		{"zombie_retains_identity", strings.Replace(exitingTaskStat, ") X 0 -1 -1 ", ") Z 10 11 12 ", 1), 0},
		{"partial_negative", strings.Replace(exitingTaskStat, "0 -1 -1", "0 -1 12", 1), 2},
		{"invalid_start", strings.Replace(exitingTaskStat, "83399945", "bad", 1), 2},
		{"malformed", "not a process stat\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stat")
			if err := os.WriteFile(path, []byte(tc.stat), 0600); err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(terminalStatScript, `stat_path="/proc/$1/stat"`, `stat_path="$1"`, 1) + "\nif read_stat \"$1\"; then exit 0; else exit $?; fi\n"
			output, err := exec.Command("/bin/sh", "-c", script, "probe", path).CombinedOutput()
			status := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					status = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if status != tc.status {
				t.Fatalf("status=%d want%d output=%s", status, tc.status, output)
			}
		})
	}
}

func TestTerminalCloseRetriesExitingSnapshotWithoutSignalling(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			sock := shortUnixSocketPath(t)
			listener, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("DOCKER_HOST", "unix://"+sock)
			t.Setenv("DOCKER_CONTEXT", "")
			root := t.TempDir()
			t.Setenv("TERMINAL_PROC_ROOT", root)
			t.Setenv("TERMINAL_STAT_PERSISTENT", fmt.Sprint(persistent))
			for pid, stat := range map[string]string{
				"1094317": exitingTaskStat,
				"4242":    strings.Replace(exitingTaskStat, "1094317 (true) X 0 -1 -1", "4242 (shell) S 1 4242 4242", 1),
				"9999":    strings.Replace(exitingTaskStat, "1094317 (true) X 0 -1 -1", "9999 (other) S 1 9999 9999", 1),
			} {
				if err := os.MkdirAll(filepath.Join(root, pid), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, pid, "stat"), []byte(stat), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Use the real scripts and close retry loop; only procfs reads and kill are
			// redirected. A second observation represents the kernel completing exit.
			installFakeDocker(t, `test "$1" = --host; shift 2; test "$1" = exec; shift; test "$1" = owned-container; shift
case "$3" in *'read_stat()'*)
 printf 'observe\n' >> "$TERMINAL_PROC_ROOT/observations"
 if [ "$(wc -l < "$TERMINAL_PROC_ROOT/observations")" -gt 1 ] && [ "$TERMINAL_STAT_PERSISTENT" = false ]; then
  rm -f "$TERMINAL_PROC_ROOT/1094317/stat" "$TERMINAL_PROC_ROOT/4242/stat"
 fi
 script=$(printf '%s' "$3" | sed "s|/proc/|$TERMINAL_PROC_ROOT/|g")
 script='kill() { printf "signal\n" >> "$TERMINAL_PROC_ROOT/signals"; return 1; }
'"$script"
 shift 3; exec /bin/sh -c "$script" "$@";;
esac
exec "$@"`)
			argv, closeSession, err := New().ExecPTYSession(context.Background(), "owned-container", nil)
			if err != nil {
				t.Fatal(err)
			}
			dir := argv[11]
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			if err := os.WriteFile(filepath.Join(dir, "pid"), []byte("4242 83399945 4242 4242"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if persistent {
				monitorDone := make(chan struct{})
				go func() {
					defer close(monitorDone)
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						raw, _ := os.ReadFile(filepath.Join(root, "observations"))
						if strings.Count(string(raw), "observe") >= 2 {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
						}
					}
				}()
				defer func() { cancel(); <-monitorDone }()
			}
			err = closeSession(ctx)
			if persistent {
				if err == nil || ctx.Err() == nil {
					t.Fatalf("unknown snapshot must remain bounded/retryable: %v context=%v", err, ctx.Err())
				}
				if _, statErr := os.Stat(filepath.Join(dir, "pid")); statErr != nil {
					t.Fatalf("unknown identity lost ownership: %v", statErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
					t.Fatalf("completed close left guard: %v", statErr)
				}
			}
			raw, readErr := os.ReadFile(filepath.Join(root, "observations"))
			if readErr != nil || strings.Count(string(raw), "observe") < 2 {
				t.Fatalf("no subsequent observation: %q %v", raw, readErr)
			}
			if _, statErr := os.Stat(filepath.Join(root, "signals")); !os.IsNotExist(statErr) {
				t.Fatalf("signalled uncertain/unrelated identity: %v", statErr)
			}
		})
	}
}
