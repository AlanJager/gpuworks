package udocker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Setting the runtime up is retried by every caller that needs it: status
// snapshots, image checks, deployments. A setup that fails for a reason the
// node has to fix (a filesystem that refuses a chown, a missing tool) would
// then run, and fail, many times a minute. After a failure the cause is
// served from memory until the backoff passes, which starts short so a fix is
// picked up soon and doubles to a ceiling so a broken node stays quiet.
const (
	provisionBackoffMin = 30 * time.Second
	provisionBackoffMax = 5 * time.Minute
)

// provisionTracker is the driver's memory of its setup attempts. Its own lock
// lets ProvisionState answer while a setup holds provisionMu for minutes.
type provisionTracker struct {
	mu          sync.Mutex
	now         func() time.Time // nil means time.Now
	done        bool
	user        string
	uid, gid    int
	fallback    string
	failure     error // the wrapped error callers receive
	cause       string
	failedAt    time.Time
	retryAt     time.Time
	consecutive int
}

func (t *provisionTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// cached is the remembered failure while its backoff runs, else nil.
func (t *provisionTracker) cached() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failure != nil && t.clock().Before(t.retryAt) {
		return t.failure
	}
	return nil
}

func (t *provisionTracker) failed(runtimeName string, err error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	wrapped := &runtime.UnavailableError{Runtime: runtimeName, Err: err}
	backoff := provisionBackoffMax
	if t.consecutive < 10 {
		backoff = min(provisionBackoffMin<<t.consecutive, provisionBackoffMax)
	}
	t.consecutive++
	t.failedAt = t.clock()
	t.retryAt = t.failedAt.Add(backoff)
	t.failure, t.cause = wrapped, err.Error()
	return wrapped
}

func (t *provisionTracker) succeeded(user string, uid, gid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done, t.user, t.uid, t.gid = true, user, uid, gid
	t.failure, t.cause, t.failedAt, t.retryAt, t.consecutive = nil, "", time.Time{}, time.Time{}, 0
}

func (t *provisionTracker) setFallback(reason string) {
	t.mu.Lock()
	t.fallback = reason
	t.mu.Unlock()
}

// ensureProvisioned prepares the runtime once. A failure is remembered and
// returned to later callers until its backoff passes, wrapped in
// runtime.UnavailableError so they can tell the runtime from the operation
// they asked for; a cancelled attempt says nothing about the node and is not
// remembered.
func (d *Driver) ensureProvisioned(ctx context.Context) error {
	d.provisionMu.Lock()
	defer d.provisionMu.Unlock()
	if d.provisioned {
		return d.ensureStateTraversal()
	}
	if err := d.track.cached(); err != nil {
		return err
	}
	d.track.setFallback("")
	if err := d.provisionRuntime(ctx); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return d.track.failed(d.launcherName(), err)
	}
	d.track.succeeded(d.runUser, d.runUID, d.runGID)
	return nil
}

// ProvisionState reports the setup without starting it.
func (d *Driver) ProvisionState() runtime.ProvisionState {
	t := &d.track
	t.mu.Lock()
	defer t.mu.Unlock()
	return runtime.ProvisionState{
		Runtime:     d.launcherName(),
		Provisioned: t.done, RunUser: t.user, RunUID: t.uid, RunGID: t.gid,
		FallbackReason: t.fallback, LastError: t.cause, LastErrorAt: t.failedAt, RetryAt: t.retryAt,
		Probe: fmt.Sprintf("set up the %s runtime: run user, %s and %s, patched PRoot", d.launcherName(), d.udockerDir, d.stateDir),
		Recheck: "findmnt -T " + shellWord(d.udockerDir) + "; ls -ld " +
			shellWord(d.udockerDir) + " " + shellWord(d.stateDir),
	}
}

// fallbackReason words why the dedicated run user was not used. cause is the
// failed probe, nil when there was no interpreter to probe with.
func fallbackReason(cause error) string {
	if cause == nil {
		return "no python interpreter to check the dedicated user with"
	}
	return strings.TrimSpace(cause.Error())
}

// shellWord quotes a path for the recheck command when it needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
