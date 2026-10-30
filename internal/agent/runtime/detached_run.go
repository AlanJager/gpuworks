package runtime

import (
	"context"
	"errors"
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

	// CreatedID consumes a per-run creation record after the CLI exits. When
	// supplied, failed runs are reclaimed only by that ID, never by name.
	// It also releases any temporary recording resources on success or failure.
	CreatedID func() (string, error)
	// OnComplete runs after the CLI and any cleanup finish, including in the
	// background. A cleanup error retains the created ID for a later retry;
	// an empty ID with an error means creation could not be ruled out.
	OnComplete func(createdID string, cleanupErr error)

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
	if err := ctx.Err(); err != nil {
		if r.CreatedID != nil {
			_, recordErr := r.CreatedID()
			err = errors.Join(err, recordErr)
		}
		r.complete("", nil)
		return "", fmt.Errorf("%s run: %w", r.CLI, err)
	}
	if err := cmd.Start(); err != nil {
		if r.CreatedID != nil {
			_, recordErr := r.CreatedID()
			err = errors.Join(err, recordErr)
		}
		r.complete("", nil)
		return "", fmt.Errorf("%s run: %w", r.CLI, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		createdID := ""
		var recordErr error
		if r.CreatedID != nil {
			createdID, recordErr = r.CreatedID()
			err = errors.Join(err, recordErr)
		}
		if err != nil {
			message := strings.TrimSpace(stderr.String())
			slog.Warn("runtime command failed", "runtime", r.CLI, "args", loggedArgs, "err", err, "stderr_tail", tailText(message, 400))
			cleanupErr := errors.Join(recordErr, removeCreatedRun(r, createdID))
			if r.CreatedID != nil && createdID == "" && runWasKilled(err) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s run client was killed before container creation could be confirmed", r.CLI))
			}
			r.complete(createdID, cleanupErr)
			return "", errors.Join(fmt.Errorf("%s run: %w: %s", r.CLI, err, message), cleanupErr)
		}
		id := strings.TrimSpace(stdout.String())
		if id == "" {
			cleanupErr := removeCreatedRun(r, createdID)
			r.complete(createdID, cleanupErr)
			return "", errors.Join(fmt.Errorf("%s run returned an empty container id", r.CLI), cleanupErr)
		}
		r.complete("", nil)
		return id, nil
	case <-timer.C:
		slog.Warn("runtime run timed out; reclaiming its container in the background", "runtime", r.CLI, "container", r.Name, "timeout", timeout)
		go reapAbandonedRun(r, cmd, done, &stdout)
		message := fmt.Sprintf("%s run timed out after %s: container creation has not completed; cleanup continues in the background", r.CLI, timeout)
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			message += "; runtime stderr: " + tailText(detail, 400)
		}
		return "", errors.New(message)
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
	killed := false
	select {
	case err := <-done:
		// The id is exact; the name could in principle already belong to a
		// newer container, so it is only the fallback when no id was printed.
		killed = runWasKilled(err)
		if id := strings.TrimSpace(stdout.String()); err == nil && id != "" {
			target = id
		}
	case <-time.After(grace):
		slog.Warn("abandoned runtime run still has not returned; killing it", "runtime", r.CLI, "container", r.Name, "grace", grace)
		_ = cmd.Process.Kill()
		<-done
		killed = true
	}
	if r.CreatedID != nil {
		createdID, err := r.CreatedID()
		if err != nil {
			slog.Warn("could not read the abandoned run's creation record", "runtime", r.CLI, "container", r.Name, "err", err)
			r.complete("", err)
			return
		}
		// A failed name collision did not create a container. Never let a
		// delayed cleanup remove the existing owner of that name.
		if createdID != "" {
			target = createdID
		} else if target == r.Name {
			if killed {
				r.complete("", fmt.Errorf("%s run client was killed before container creation could be confirmed", r.CLI))
			} else {
				r.complete("", nil)
			}
			return
		}
	}
	if r.Remove == nil {
		r.complete("", nil)
		return
	}
	if err := r.Remove(context.Background(), target); err != nil {
		r.complete(target, err)
		slog.Warn("could not remove the container of an abandoned run; it may remain", "runtime", r.CLI, "container", r.Name, "target", target, "err", err)
		return
	}
	r.complete("", nil)
	slog.Info("removed the container of an abandoned run", "runtime", r.CLI, "container", r.Name, "target", target)
}

func runWasKilled(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ProcessState != nil && exitErr.ProcessState.ExitCode() < 0
}

func (r DetachedRun) complete(id string, err error) {
	if r.OnComplete != nil {
		r.OnComplete(id, err)
	}
}

func removeCreatedRun(r DetachedRun, id string) error {
	if id == "" || r.Remove == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Remove(ctx, id); err != nil {
		return fmt.Errorf("remove failed %s container %s: %w", r.CLI, id, err)
	}
	return nil
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
