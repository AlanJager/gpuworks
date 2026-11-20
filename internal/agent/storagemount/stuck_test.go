package storagemount

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stuckClock is the manager's clock in these tests: time passes only when a
// test says so, so no test waits for the remount threshold.
type stuckClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStuckClock() *stuckClock {
	return &stuckClock{now: time.Date(2026, 11, 17, 9, 50, 27, 0, time.UTC)}
}

func (c *stuckClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stuckClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

const sinceText = "no answer since 2026-11-17T09:50:27Z"

// stuckNFS mounts an export through the manager and then hangs its statfs,
// as a hard NFS mount whose server went away does. The returned function
// lets the calls blocked so far return; the test's cleanup calls it too.
func stuckNFS(t *testing.T, e *env, mut ...func(*Options)) (*Manager, *stuckClock, Spec, func()) {
	t.Helper()
	clock := newStuckClock()
	e.h.exports["nas:/m"] = fakeExport{}
	m := e.manager(append([]func(*Options){func(o *Options) {
		o.Now = clock.Now
		o.StuckRemountAfter = 3 * time.Minute
	}}, mut...)...)
	sp := e.nfsSpec(1, "m", "nas:/m")
	wantState(t, one(t, m, sp), StateReady, "")
	release := e.sf.blockPath(sp.Path)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return m, clock, sp, unblock
}

func forcedUmounts(h *fakeHost) int {
	n := 0
	for _, call := range h.callsTo("umount") {
		if len(call) == 4 && call[1] == "-f" && call[2] == "-l" {
			n++
		}
	}
	return n
}

// GWKS-383: a mount whose call never returns kept its storage unresponsive
// for good. The agent now says since when and what it will do, waits out the
// threshold, forces its own mount off and mounts the export again, although
// the first call is still blocked.
func TestStuckMountIsForcedOffAndMountedAgainWhileItsCallNeverReturns(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, _ := stuckNFS(t, e)
	// The forced unmount takes the filesystem away; calls made after it reach
	// the new mount, while the one blocked before stays blocked.
	e.h.forced = func(path string) { e.sf.letThrough(path) }

	// The first check cannot tell a hang from a slow answer yet.
	st := one(t, m, sp)
	wantState(t, st, StateUnresponsive, ReasonUnresponsiveRetrying)
	if !strings.Contains(st.Message, "statfs: filesystem did not respond") || !strings.Contains(st.Message, sinceText) {
		t.Fatalf("message = %q", st.Message)
	}
	clock.advance(2*time.Minute + 59*time.Second)
	st = one(t, m, sp)
	wantState(t, st, StateUnresponsive, ReasonStuckWillRemount)
	if !strings.Contains(st.Message, sinceText) {
		t.Fatalf("the time it stopped answering moved: %q", st.Message)
	}
	if n := forcedUmounts(e.h); n != 0 {
		t.Fatalf("forced unmount ran %d times before the threshold", n)
	}

	clock.advance(time.Second)
	st = one(t, m, sp)
	wantState(t, st, StateReady, "")
	if st.Message != "" || st.TotalBytes == 0 {
		t.Fatalf("recovered status = %+v", st)
	}
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("forced unmount ran %d times", n)
	}
	if n := len(e.h.callsTo("mount")); n != 2 || !e.h.mounted(sp.Path) {
		t.Fatalf("mount ran %d times, mounted=%v", n, e.h.mounted(sp.Path))
	}
	// It stays ready, and a later hang is timed from its own beginning.
	wantState(t, one(t, m, sp), StateReady, "")
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("a ready storage was forced off again (%d)", n)
	}
}

// A call that returns once the server answers again frees its key by itself:
// the storage is ready on the next check and nothing is unmounted.
func TestStuckMountRecoversByItselfWhenItsCallReturns(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, release := stuckNFS(t, e)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonUnresponsiveRetrying)
	clock.advance(time.Minute)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckWillRemount)
	e.sf.letThrough(sp.Path)
	release()
	eventually(t, "the blocked call to return", func() bool { return one(t, m, sp).State == StateReady })
	if n := forcedUmounts(e.h); n != 0 {
		t.Fatalf("forced unmount ran %d times", n)
	}
	// The next hang starts its own clock: the minute already spent is gone.
	again := e.sf.blockPath(sp.Path)
	t.Cleanup(func() { close(again) })
	clock.advance(2*time.Minute + 30*time.Second)
	one(t, m, sp)
	clock.advance(2*time.Minute + 59*time.Second)
	st := one(t, m, sp)
	wantState(t, st, StateUnresponsive, ReasonStuckWillRemount)
	if !strings.Contains(st.Message, "no answer since 2026-11-17T09:53:57Z") || forcedUmounts(e.h) != 0 {
		t.Fatalf("message = %q, forced = %d", st.Message, forcedUmounts(e.h))
	}
}

// A mount a running instance uses is left alone however long it hangs, and
// forced off once the instance is gone.
func TestStuckMountInUseIsReportedAndNotForcedOff(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	inUse := true
	e.busy = func(string) bool { mu.Lock(); defer mu.Unlock(); return inUse }
	m, clock, sp, _ := stuckNFS(t, e)
	e.h.forced = func(path string) { e.sf.letThrough(path) }
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonUnresponsiveRetrying)
	clock.advance(time.Hour)
	st := one(t, m, sp)
	wantState(t, st, StateUnresponsive, ReasonStuckInUse)
	if n := forcedUmounts(e.h); n != 0 || !e.h.mounted(sp.Path) || !strings.Contains(st.Message, sinceText) {
		t.Fatalf("forced=%d mounted=%v message=%q", n, e.h.mounted(sp.Path), st.Message)
	}
	mu.Lock()
	inUse = false
	mu.Unlock()
	wantState(t, one(t, m, sp), StateReady, "")
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("forced unmount ran %d times once the instance was gone", n)
	}
}

// What the agent did not mount is never unmounted: an existing mount used as
// it is, and an export someone else mounted on the storage's path.
func TestStuckMountTheAgentDidNotCreateIsNeverForcedOff(t *testing.T) {
	for _, existing := range []bool{true, false} {
		e := newEnv(t)
		clock := newStuckClock()
		m := e.manager(func(o *Options) { o.Now = clock.Now })
		sp := e.nfsSpec(1, "m", "nas:/m")
		sp.Existing = existing
		if err := os.MkdirAll(sp.Path, 0o755); err != nil {
			t.Fatal(err)
		}
		e.h.addForeign(sp.Path, "nas:/m")
		wantState(t, one(t, m, sp), StateReady, "")
		release := e.sf.blockPath(sp.Path)
		t.Cleanup(func() { close(release) })
		clock.advance(time.Hour)
		wantState(t, one(t, m, sp), StateUnresponsive, ReasonUnresponsiveRetrying)
		clock.advance(time.Hour)
		for range 3 {
			st := one(t, m, sp)
			wantState(t, st, StateUnresponsive, ReasonStuckNotManaged)
			if !strings.Contains(st.Message, "no answer since 2026-11-17T10:50:27Z") {
				t.Fatalf("existing=%v message=%q", existing, st.Message)
			}
			clock.advance(time.Hour)
		}
		if n := len(e.h.callsTo("umount")); n != 0 || !e.h.mounted(sp.Path) {
			t.Fatalf("existing=%v: umount ran %d times, mounted=%v", existing, n, e.h.mounted(sp.Path))
		}
	}
}

// A forced unmount that fails is reported and tried again only after another
// full threshold; when it then works the storage comes back.
func TestStuckMountForcedUnmountFailureIsReportedAndRetriedLater(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, _ := stuckNFS(t, e)
	e.h.forced = func(path string) { e.sf.letThrough(path) }
	e.h.forceFail = map[string]string{sp.Path: "umount.nfs4: " + sp.Path + ": device is busy"}
	one(t, m, sp)
	clock.advance(3 * time.Minute)
	st := one(t, m, sp)
	wantState(t, st, StateUnresponsive, ReasonStuckWillRemount)
	if !strings.Contains(st.Message, "forced unmount failed: umount.nfs4: "+sp.Path+": device is busy") || !e.h.mounted(sp.Path) {
		t.Fatalf("message = %q mounted=%v", st.Message, e.h.mounted(sp.Path))
	}
	clock.advance(2 * time.Minute)
	wantState(t, one(t, m, sp), StateUnresponsive, ReasonStuckWillRemount)
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("forced unmount retried after %d attempts before the threshold passed again", n)
	}
	e.h.mu.Lock()
	e.h.forceFail = nil
	e.h.mu.Unlock()
	clock.advance(time.Minute)
	wantState(t, one(t, m, sp), StateReady, "")
	if n := forcedUmounts(e.h); n != 2 {
		t.Fatalf("forced unmount ran %d times", n)
	}
}

// The server is still away after the mount was forced off: the new mount
// fails, the storage says the server cannot be reached, and every later
// check tries the mount again without another forced unmount.
func TestStuckMountWhoseRemountFailsReportsTheServer(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, _ := stuckNFS(t, e)
	e.h.forced = func(path string) {
		e.sf.letThrough(path)
		e.h.exports["nas:/m"] = fakeExport{failOutput: "mount.nfs: Connection timed out"}
	}
	one(t, m, sp)
	clock.advance(3 * time.Minute)
	st := one(t, m, sp)
	wantState(t, st, StateFailed, ReasonUnreachable)
	if e.h.mounted(sp.Path) || strings.Contains(st.Message, "no answer since") {
		t.Fatalf("status = %+v mounted=%v", st, e.h.mounted(sp.Path))
	}
	e.h.mu.Lock()
	e.h.exports["nas:/m"] = fakeExport{}
	e.h.mu.Unlock()
	wantState(t, one(t, m, sp), StateReady, "")
	if n := forcedUmounts(e.h); n != 1 {
		t.Fatalf("forced unmount ran %d times", n)
	}
}

// The calls an operation of the agent has blocked on the mount (a heartbeat,
// a scan) are given up with it, so that operation works on the new mount; a
// call that returns late does not free the key of the one that replaced it.
func TestStuckMountGivesUpTheOperationsBlockedOnIt(t *testing.T) {
	e := newEnv(t)
	m, clock, sp, _ := stuckNFS(t, e)
	e.h.forced = func(path string) { e.sf.letThrough(path) }
	held := make(chan struct{})
	old := func() error { <-held; return nil }
	if err := m.Do(sp.ID, "heartbeat", 20*time.Millisecond, old); err != ErrUnresponsive {
		t.Fatalf("blocked heartbeat = %v", err)
	}
	if err := m.Do(sp.ID, "heartbeat", time.Second, func() error { return nil }); err != ErrUnresponsive {
		t.Fatalf("a second heartbeat ran beside the blocked one: %v", err)
	}
	one(t, m, sp)
	clock.advance(3 * time.Minute)
	wantState(t, one(t, m, sp), StateReady, "")

	next := make(chan struct{})
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- m.Do(sp.ID, "heartbeat", 5*time.Second, func() error { close(started); <-next; return nil })
	}()
	<-started
	close(held) // the abandoned call returns while the new one holds the key
	eventually(t, "the abandoned call to finish", func() bool {
		return m.Do(sp.ID, "other", time.Second, func() error { return nil }) == nil
	})
	if err := m.Do(sp.ID, "heartbeat", time.Second, func() error { return nil }); err != ErrUnresponsive {
		t.Fatalf("the late return of an abandoned call freed the new call's key: %v", err)
	}
	close(next)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

// A storage that hangs on its node cache, or a local one, has no mount to
// take off: it reports since when, and nothing is unmounted.
func TestStuckCacheOrLocalDirectoryIsReportedOnly(t *testing.T) {
	e := newEnv(t)
	clock := newStuckClock()
	e.h.exports["nas:/m"] = fakeExport{}
	m := e.manager(func(o *Options) { o.Now = clock.Now })
	sp := e.nfsSpec(1, "m", "nas:/m")
	sp.Type, sp.CachePath = TypeNFSCache, e.path("cache")
	local := Spec{ID: 2, Type: TypeLocal, Path: e.path("local")}
	got := m.Reconcile(context.Background(), []Spec{sp, local})
	wantState(t, got[0], StateReady, "")
	wantState(t, got[1], StateReady, "")
	for _, p := range []string{sp.CachePath, local.Path} {
		release := e.sf.blockPath(p)
		t.Cleanup(func() { close(release) })
	}
	clock.advance(time.Hour)
	for range 2 {
		got = m.Reconcile(context.Background(), []Spec{sp, local})
		for _, st := range got {
			wantState(t, st, StateUnresponsive, "")
			if !strings.Contains(st.Message, "no answer since 2026-11-17T10:50:27Z") {
				t.Fatalf("message = %q", st.Message)
			}
		}
		clock.advance(time.Hour)
	}
	if n := len(e.h.callsTo("umount")); n != 0 {
		t.Fatalf("umount ran %d times", n)
	}
}
