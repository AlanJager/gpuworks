package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/commandlog"
)

// DefaultRunTimeout bounds how long a detached `run` may take before the
// launch is reported as failed. Creating a container is normally a few
// seconds, but the daemon unmounts a temporary overlay while preparing the
// init layer, which waits for the root filesystem's writeback: on a busy
// spinning disk this alone was measured at 88 s.
const DefaultRunTimeout = 120 * time.Second

// defaultAbandonGrace is how long a timed-out run is left to finish on its own
// before the CLI is killed.
const defaultAbandonGrace = 10 * time.Minute

// DetachedRun is one `<cli> ... run -d --name <Name> ...` invocation.
type DetachedRun struct {
	CLI  string   // executable, e.g. "docker" or "nerdctl"
	Args []string // argv after the executable
	Name string   // the --name the container is created under
	// Remove force-deletes a container by id or name and treats an absent
	// container as success. It reclaims what an abandoned run leaves behind.
	Remove func(ctx context.Context, idOrName string) error

	Timeout      time.Duration // zero means DefaultRunTimeout
	AbandonGrace time.Duration // zero means defaultAbandonGrace
}

// RunDetached executes r and returns the container id the CLI prints.
//
// When the run outlives its timeout or ctx, the launch fails but the CLI is
// not killed: the daemon keeps creating the container after its client goes
// away, and an inspect right after the kill finds nothing, so the container
// would appear later with nobody owning it. Instead the CLI is left to finish
// in the background and whatever it created is removed then — by the id it
// prints, or by name if it failed after creating the container. A CLI that has
// still not returned after the grace period is killed and the name removed.
func RunDetached(ctx context.Context, r DetachedRun) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	loggedArgs := commandlog.RedactArgv(r.Args)
	slog.Info("runtime command", "runtime", r.CLI, "args", loggedArgs)
	cmd := exec.Command(r.CLI, r.Args...)
	var stdout, stderr lockedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%s run: %w", r.CLI, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			message := strings.TrimSpace(stderr.String())
			slog.Warn("runtime command failed", "runtime", r.CLI, "args", loggedArgs, "err", err, "stderr_tail", tailText(message, 400))
			return "", fmt.Errorf("%s run: %w: %s", r.CLI, err, message)
		}
		id := strings.TrimSpace(stdout.String())
		if id == "" {
			return "", fmt.Errorf("%s run returned an empty container id", r.CLI)
		}
		return id, nil
	case <-timer.C:
		slog.Warn("runtime run timed out; reclaiming its container in the background", "runtime", r.CLI, "container", r.Name, "timeout", timeout)
		go reapAbandonedRun(r, cmd, done, &stdout)
		return "", fmt.Errorf("%s run timed out after %s: the container runtime did not finish creating the container (daemon slow or overloaded); it is removed once the runtime returns", r.CLI, timeout)
	case <-ctx.Done():
		slog.Info("runtime run cancelled; reclaiming its container in the background", "runtime", r.CLI, "container", r.Name)
		go reapAbandonedRun(r, cmd, done, &stdout)
		return "", fmt.Errorf("%s run: %w", r.CLI, ctx.Err())
	}
}

func reapAbandonedRun(r DetachedRun, cmd *exec.Cmd, done <-chan error, stdout *lockedBuffer) {
	grace := r.AbandonGrace
	if grace <= 0 {
		grace = defaultAbandonGrace
	}
	target := r.Name
	select {
	case err := <-done:
		// The id is exact; the name could in principle already belong to a
		// newer container, so it is only the fallback when no id was printed.
		if id := strings.TrimSpace(stdout.String()); err == nil && id != "" {
			target = id
		}
	case <-time.After(grace):
		slog.Warn("abandoned runtime run still has not returned; killing it", "runtime", r.CLI, "container", r.Name, "grace", grace)
		_ = cmd.Process.Kill()
		<-done
	}
	if r.Remove == nil {
		return
	}
	if err := r.Remove(context.Background(), target); err != nil {
		slog.Warn("could not remove the container of an abandoned run; it may remain", "runtime", r.CLI, "container", r.Name, "target", target, "err", err)
		return
	}
	slog.Info("removed the container of an abandoned run", "runtime", r.CLI, "container", r.Name, "target", target)
}

// lockedBuffer lets the reaper read the CLI's output while exec's copy
// goroutine may still be writing it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func tailText(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
