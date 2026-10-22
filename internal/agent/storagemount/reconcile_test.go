package storagemount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func one(t *testing.T, m *Manager, sp Spec) Status {
	t.Helper()
	got := m.Reconcile(context.Background(), []Spec{sp})
	if len(got) != 1 {
		t.Fatalf("got %d statuses", len(got))
	}
	return got[0]
}

func wantState(t *testing.T, st Status, state, reason string) {
	t.Helper()
	if st.State != state || st.Reason != reason {
		t.Fatalf("state = %s/%s (%s), want %s/%s", st.State, st.Reason, st.Message, state, reason)
	}
}

func TestReconcileLocal(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	p := e.path("a/b/models")
	st := one(t, m, Spec{ID: 1, Name: "local", Type: TypeLocal, Path: p})
	wantState(t, st, StateReady, "")
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if !st.Writable || st.TotalBytes != 100<<30 || st.AvailableBytes != 40<<30 || st.Path != p {
		t.Errorf("status = %+v", st)
	}
	if ents, _ := os.ReadDir(p); len(ents) != 0 {
		t.Errorf("the write probe must clean up after itself: %v", ents)
	}
	if len(e.h.callsTo("mount")) != 0 {
		t.Error("local storage must never run mount")
	}
}

func TestReconcileLocalFailures(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	file := e.path("file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st := one(t, m, Spec{ID: 1, Type: TypeLocal, Path: filepath.Join(file, "sub")})
	if st.State != StateFailed {
		t.Fatalf("directory below a regular file: %+v", st)
	}
	st = one(t, m, Spec{ID: 2, Type: TypeLocal, Path: "relative/dir"})
	wantState(t, st, StateFailed, ReasonOther)

	e.sf.failFor[e.path("sf")] = errors.New("input/output error")
	st = one(t, m, Spec{ID: 3, Type: TypeLocal, Path: e.path("sf")})
	wantState(t, st, StateFailed, ReasonOther)
	if !strings.Contains(st.Message, "input/output error") {
		t.Errorf("message = %q", st.Message)
	}

	st = one(t, m, Spec{ID: 4, Type: "s3", Path: e.path("x")})
	wantState(t, st, StateUnsupported, ReasonOther)
}

func TestReconcileLocalReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	p := e.path("ro")
	if err := os.Mkdir(p, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o755) })
	st := one(t, e.manager(), Spec{ID: 1, Type: TypeLocal, Path: p})
	wantState(t, st, StateReady, "")
	if st.Writable {
		t.Error("unwritable directory must report Writable=false but stay ready")
	}
}

func TestReconcileSharedDirAndExisting(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	dir := e.path("shared")
	if err := os.MkdirAll(filepath.Join(dir, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := one(t, m, Spec{ID: 1, Type: TypeSharedDir, Path: dir})
	wantState(t, st, StateReady, "")
	if !st.Writable || st.TotalBytes == 0 {
		t.Errorf("status = %+v", st)
	}

	// Existing NFS spec: the mount at the path is used as is, nothing is mounted.
	e.h.addForeign(dir, "nas:/models")
	st = one(t, m, Spec{ID: 2, Type: TypeNFS, Path: dir, Export: "nas:/models", Existing: true})
	wantState(t, st, StateReady, "")
	if n := len(e.h.callsTo("mount")); n != 0 {
		t.Errorf("Existing must never mount, mount ran %d times", n)
	}

	st = one(t, m, Spec{ID: 3, Type: TypeSharedDir, Path: e.path("nope")})
	wantState(t, st, StateFailed, ReasonPathMissing)
	if _, err := os.Stat(e.path("nope")); err == nil {
		t.Error("shared_dir must not create its directory")
	}

	f := e.path("afile")
	os.WriteFile(f, nil, 0o644)
	st = one(t, m, Spec{ID: 4, Type: TypeSharedDir, Path: f})
	wantState(t, st, StateFailed, ReasonPathMissing)

	st = one(t, m, Spec{ID: 5, Type: TypeNFSCache, Path: e.path("nope2"), Export: "nas:/m", Existing: true})
	wantState(t, st, StateFailed, ReasonPathMissing)
}

func TestReconcileSharedDirPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	dir := e.path("locked")
	os.Mkdir(dir, 0o000)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	st := one(t, e.manager(), Spec{ID: 1, Type: TypeSharedDir, Path: dir})
	wantState(t, st, StateFailed, ReasonPermission)
}

func TestReconcileNFSMountsOnce(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas.example:/models"] = fakeExport{}
	state := e.path("state/mounts.json")
	m := e.manager(func(o *Options) { o.StateFile = state })
	sp := Spec{ID: 7, Name: "models", Type: TypeNFS, Path: e.path("mnt/models"), Export: "nas.example:/models",
		MountOptions: "vers=4.1,hard"}

	st := one(t, m, sp)
	wantState(t, st, StateReady, "")
	if !st.Writable || st.AvailableBytes != 40<<30 {
		t.Errorf("status = %+v", st)
	}
	calls := e.h.callsTo("mount")
	want := []string{"mount", "-t", "nfs", "-o", "rw,vers=4.1,hard,noatime", "nas.example:/models", sp.Path}
	if len(calls) != 1 || strings.Join(calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("mount calls = %q, want one call %q", calls, want)
	}
	if !e.h.mounted(sp.Path) {
		t.Fatal("not mounted")
	}
	if ents, _ := os.ReadDir(filepath.Join(sp.Path, ".gpuworks")); len(ents) != 0 {
		t.Errorf("probe file left behind: %v", ents)
	}

	// Second reconcile sees the mount in mountinfo and does nothing; a trailing slash on the
	// export does not make the source "different".
	sp.Export = "nas.example:/models/"
	e.h.exports["nas.example:/models/"] = fakeExport{}
	st = one(t, m, sp)
	wantState(t, st, StateReady, "")
	if n := len(e.h.callsTo("mount")); n != 1 {
		t.Errorf("mount ran %d times, want 1", n)
	}

	b, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(b), sp.Path) || !strings.Contains(string(b), "nas.example:/models") {
		t.Errorf("state file = %q, %v", b, err)
	}
}

func TestReconcileNFSDefaultOptions(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/a"] = fakeExport{}
	one(t, e.manager(), e.nfsSpec(1, "a", "nas:/a"))
	c := e.h.callsTo("mount")
	if len(c) != 1 || c[0][4] != "rw,noatime" {
		t.Fatalf("calls = %q", c)
	}
}

func TestReconcileNFSMountFailures(t *testing.T) {
	cases := []struct {
		name, output, reason string
	}{
		{"missing client", "mount: /mnt/x: bad option; for several filesystems (e.g. nfs, cifs) you might need a /sbin/mount.<type> helper program.", ReasonMissingClient},
		{"denied", "mount.nfs: access denied by server while mounting nas:/m", ReasonPermission},
		{"timeout", "mount.nfs: Connection timed out", ReasonUnreachable},
		{"no route", "mount.nfs: No route to host", ReasonUnreachable},
		{"dns", "mount.nfs: Failed to resolve server nfs.example: Name or service not known", ReasonUnreachable},
		{"refused", "mount.nfs: Connection refused", ReasonUnreachable},
		{"unknown", "mount.nfs: something strange", ReasonOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.h.exports["nas:/m"] = fakeExport{failOutput: c.output}
			state := e.path("mounts.json")
			m := e.manager(func(o *Options) { o.StateFile = state })
			st := one(t, m, e.nfsSpec(1, "m", "nas:/m"))
			wantState(t, st, StateFailed, c.reason)
			if !strings.Contains(st.Message, c.output) {
				t.Errorf("message %q must carry the command output", st.Message)
			}
			if st.Writable || st.TotalBytes != 0 {
				t.Errorf("a failed storage reports no capacity: %+v", st)
			}
			if _, err := os.Stat(state); err == nil {
				t.Error("a failed mount must not be recorded as ours")
			}
			// The next reconcile retries.
			e.h.exports["nas:/m"] = fakeExport{}
			wantState(t, one(t, m, e.nfsSpec(1, "m", "nas:/m")), StateReady, "")
		})
	}
}

func TestReconcileNFSMissingExportOnServer(t *testing.T) {
	e := newEnv(t)
	st := one(t, e.manager(), e.nfsSpec(1, "nope", "nas:/nope"))
	wantState(t, st, StateFailed, ReasonPathMissing)
}

func TestReconcileNFSMessageTruncated(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{failOutput: strings.Repeat("x", 5000)}
	st := one(t, e.manager(), e.nfsSpec(1, "m", "nas:/m"))
	if len(st.Message) > 400+len("mount failed: ")+3 {
		t.Errorf("message has %d bytes", len(st.Message))
	}
}

func TestReconcileNFSNotRoot(t *testing.T) {
	e := newEnv(t)
	e.root = false
	e.h.exports["nas:/m"] = fakeExport{}
	m := e.manager()
	st := one(t, m, e.nfsSpec(1, "m", "nas:/m"))
	wantState(t, st, StateUnsupported, ReasonNotRoot)
	if !strings.Contains(st.Message, "shared directory") {
		t.Errorf("message should point at shared directories: %q", st.Message)
	}
	if len(e.h.calls) != 0 {
		t.Errorf("no command may run without root: %v", e.h.calls)
	}
	// An administrator-provided mount works for a non-root agent.
	e.h.addForeign(e.path("m"), "nas:/m")
	wantState(t, one(t, m, e.nfsSpec(1, "m", "nas:/m")), StateReady, "")
}

func TestReconcileNFSSourceMismatch(t *testing.T) {
	e := newEnv(t)
	other := e.path("m")
	os.MkdirAll(other, 0o755)
	e.h.addForeign(other, "other-nas:/data")
	state := e.path("mounts.json")
	m := e.manager(func(o *Options) { o.StateFile = state })
	e.h.exports["nas:/m"] = fakeExport{}
	st := one(t, m, e.nfsSpec(1, "m", "nas:/m"))
	wantState(t, st, StateFailed, ReasonSourceMismatch)
	if !strings.Contains(st.Message, "other-nas:/data") || !strings.Contains(st.Message, "nas:/m") {
		t.Errorf("message must name both sources: %q", st.Message)
	}
	if len(e.h.calls) != 0 || !e.h.mounted(other) {
		t.Errorf("the foreign mount must be left alone, calls = %v", e.h.calls)
	}
	// And later removing the spec must not unmount it either.
	m.Reconcile(context.Background(), nil)
	if len(e.h.callsTo("umount")) != 0 {
		t.Error("umount must not run for a mount this agent did not create")
	}
}

func TestReconcileNFSMountedAsNonNFS(t *testing.T) {
	e := newEnv(t)
	p := e.path("m")
	os.MkdirAll(p, 0o755)
	// A tmpfs whose source happens to look like the export is still a mismatch.
	line := "90 22 0:90 / " + p + " rw - tmpfs nas:/m rw\n"
	b, _ := os.ReadFile(e.h.infoPath)
	os.WriteFile(e.h.infoPath, append(b, line...), 0o644)
	st := one(t, e.manager(), e.nfsSpec(1, "m", "nas:/m"))
	wantState(t, st, StateFailed, ReasonSourceMismatch)
}

func TestReconcileNFSReadOnlyExport(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/ro"] = fakeExport{readOnly: true}
	st := one(t, e.manager(), Spec{ID: 1, Type: TypeNFS, Path: e.path("ro"), Export: "nas:/ro", MountOptions: "ro"})
	wantState(t, st, StateReady, "")
	if st.Writable {
		t.Error("a read-only export is ready but not writable")
	}
	if c := e.h.callsTo("mount"); c[0][4] != "ro,noatime" {
		t.Errorf("options = %q", c[0][4])
	}
}

func TestReconcileNFSInvalidInput(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	for _, sp := range map[string]Spec{
		"bad option":    {ID: 1, Type: TypeNFS, Path: e.path("a"), Export: "nas:/a", MountOptions: "rw;reboot"},
		"empty export":  {ID: 2, Type: TypeNFS, Path: e.path("b")},
		"dash export":   {ID: 3, Type: TypeNFS, Path: e.path("c"), Export: "-o"},
		"no host":       {ID: 4, Type: TypeNFS, Path: e.path("d"), Export: "/just/a/path"},
		"space":         {ID: 5, Type: TypeNFS, Path: e.path("e"), Export: "nas:/a b"},
		"relative path": {ID: 6, Type: TypeNFS, Path: "mnt/x", Export: "nas:/a"},
	} {
		wantState(t, one(t, m, sp), StateFailed, ReasonOther)
	}
	if len(e.h.callsTo("mount")) != 0 {
		t.Errorf("invalid input must never reach mount: %v", e.h.calls)
	}
}

func TestReconcileNFSMountpointCannotBeCreated(t *testing.T) {
	e := newEnv(t)
	f := e.path("file")
	os.WriteFile(f, nil, 0o644)
	st := one(t, e.manager(), Spec{ID: 1, Type: TypeNFS, Path: filepath.Join(f, "mnt"), Export: "nas:/a"})
	if st.State != StateFailed {
		t.Fatalf("status = %+v", st)
	}
	if len(e.h.callsTo("mount")) != 0 {
		t.Error("mount must not run when the mount point cannot be created")
	}
}

func TestReconcileNFSCache(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{}
	m := e.manager()
	cache := e.path("cache/m")
	sp := Spec{ID: 1, Name: "m", Type: TypeNFSCache, Path: e.path("m"), Export: "nas:/m", CachePath: cache}
	st := one(t, m, sp)
	wantState(t, st, StateReady, "")
	if fi, err := os.Stat(cache); err != nil || !fi.IsDir() {
		t.Fatal("cache directory not created")
	}
	if st.CachePath != cache || st.CacheTotalBytes != 100<<30 || st.CacheAvailableBytes != 40<<30 {
		t.Errorf("cache fields = %+v", st)
	}
	if e.sf.count(cache) != 1 {
		t.Errorf("cache statfs calls = %d", e.sf.count(cache))
	}

	sp.ID, sp.Path, sp.CachePath = 2, e.path("m2"), ""
	e.h.exports["nas:/m"] = fakeExport{}
	st = one(t, m, sp)
	// The agent has no default cache directory: the server always sends one.
	wantState(t, st, StateFailed, ReasonPathMissing)
	if st.Writable || st.CacheTotalBytes != 0 {
		t.Errorf("empty CachePath must fail the storage: %+v", st)
	}

	file := e.path("file")
	os.WriteFile(file, nil, 0o644)
	sp.ID, sp.Path, sp.CachePath = 3, e.path("m3"), filepath.Join(file, "cache")
	st = one(t, m, sp)
	if st.State != StateFailed || !strings.Contains(st.Message, "cache") {
		t.Errorf("cache directory failure must fail the storage: %+v", st)
	}
}

func TestReconcileUnresponsiveStatfs(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{}
	m := e.manager()
	sp := e.nfsSpec(1, "m", "nas:/m")
	wantState(t, one(t, m, sp), StateReady, "")

	release := e.sf.blockPath(sp.Path)
	start := time.Now()
	st := one(t, m, sp)
	wantState(t, st, StateUnresponsive, "")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("reconcile took %s with a 300ms call timeout", d)
	}
	callsBefore := e.sf.count(sp.Path)

	// While the first statfs is stuck nothing new is started, however often we reconcile.
	for i := 0; i < 5; i++ {
		begin := time.Now()
		wantState(t, one(t, m, sp), StateUnresponsive, "")
		if time.Since(begin) > 100*time.Millisecond {
			t.Fatal("repeat reconcile of a stuck storage must return immediately")
		}
	}
	if n := e.sf.count(sp.Path); n != callsBefore {
		t.Fatalf("statfs started %d more times while stuck", n-callsBefore)
	}

	close(release)
	eventually(t, "storage recovery", func() bool {
		return one(t, m, sp).State == StateReady
	})
}

func TestReconcileStuckStorageDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/a"] = fakeExport{}
	e.h.exports["nas:/b"] = fakeExport{}
	m := e.manager()
	a, b := e.nfsSpec(1, "a", "nas:/a"), e.nfsSpec(2, "b", "nas:/b")
	local := Spec{ID: 3, Type: TypeLocal, Path: e.path("local")}
	release := e.sf.blockPath(a.Path)
	defer close(release)

	start := time.Now()
	got := m.Reconcile(context.Background(), []Spec{a, b, local})
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("reconcile took %s", d)
	}
	wantState(t, got[0], StateUnresponsive, "")
	wantState(t, got[1], StateReady, "")
	wantState(t, got[2], StateReady, "")
	if got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Errorf("results must keep spec order: %+v", got)
	}
}

func TestReconcileHungMountIsTrackedWhenItFinishesLate(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{}
	release := make(chan struct{})
	e.h.mountHook = func(context.Context) { <-release } // ignores ctx, like a process stuck in D state
	state := e.path("mounts.json")
	m := e.manager(func(o *Options) {
		o.StateFile = state
		o.MountTimeout = 100 * time.Millisecond
		o.CallTimeout = 100 * time.Millisecond
	})
	sp := e.nfsSpec(1, "m", "nas:/m")
	start := time.Now()
	st := one(t, m, sp)
	wantState(t, st, StateUnresponsive, ReasonUnreachable)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %s", d)
	}
	if n := len(e.h.callsTo("mount")); n != 1 {
		t.Fatalf("mount ran %d times", n)
	}
	// Reconciling again while the mount is stuck does not start another mount.
	wantState(t, one(t, m, sp), StateUnresponsive, "")
	if n := len(e.h.callsTo("mount")); n != 1 {
		t.Fatalf("mount ran %d times while stuck", n)
	}

	close(release)
	eventually(t, "late mount recorded", func() bool {
		b, _ := os.ReadFile(state)
		return strings.Contains(string(b), sp.Path)
	})
	wantStateEventually := func() bool { return one(t, m, sp).State == StateReady }
	eventually(t, "ready after late mount", wantStateEventually)
	if n := len(e.h.callsTo("mount")); n != 1 {
		t.Errorf("mount ran %d times, want 1", n)
	}
}

func TestReconcileMountTimeoutHonouredByRunner(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{}
	// A well behaved mount command: the context kill ends it with no output and no mount.
	m := e.manager(func(o *Options) {
		o.MountTimeout = 100 * time.Millisecond
		o.Runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, errors.New("signal: killed")
		}
	})
	st := one(t, m, e.nfsSpec(1, "m", "nas:/m"))
	wantState(t, st, StateFailed, ReasonUnreachable)
	if !strings.Contains(st.Message, "did not finish") {
		t.Errorf("message = %q", st.Message)
	}
}

func TestReconcileUnmountRemovedStorage(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/a"] = fakeExport{}
	e.h.exports["nas:/b"] = fakeExport{}
	state := e.path("mounts.json")
	m := e.manager(func(o *Options) { o.StateFile = state })
	a, b := e.nfsSpec(1, "a", "nas:/a"), e.nfsSpec(2, "b", "nas:/b")
	m.Reconcile(context.Background(), []Spec{a, b})
	if !e.h.mounted(a.Path) || !e.h.mounted(b.Path) {
		t.Fatal("setup: both must be mounted")
	}

	got := m.Reconcile(context.Background(), []Spec{b})
	if len(got) != 1 {
		t.Fatal(got)
	}
	if e.h.mounted(a.Path) || !e.h.mounted(b.Path) {
		t.Errorf("only the removed storage must be unmounted (a=%v b=%v)", e.h.mounted(a.Path), e.h.mounted(b.Path))
	}
	um := e.h.callsTo("umount")
	if len(um) != 1 || len(um[0]) != 2 || um[0][1] != a.Path {
		t.Errorf("umount calls = %q; want a single plain `umount <path>`", um)
	}
	if bs, _ := os.ReadFile(state); strings.Contains(string(bs), a.Path) || !strings.Contains(string(bs), b.Path) {
		t.Errorf("state file = %s", bs)
	}
}

func TestReconcileUnmountSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/a"] = fakeExport{}
	state := e.path("mounts.json")
	a := e.nfsSpec(1, "a", "nas:/a")
	one(t, e.manager(func(o *Options) { o.StateFile = state }), a)

	// A new Manager (agent restart) with the same state file still knows the mount is ours.
	m2 := e.manager(func(o *Options) { o.StateFile = state })
	m2.Reconcile(context.Background(), nil)
	if e.h.mounted(a.Path) {
		t.Error("a mount created before the restart must be unmounted when its storage is gone")
	}

	// Without a state file nothing is known, so nothing is unmounted.
	e.h.exports["nas:/b"] = fakeExport{}
	b := e.nfsSpec(2, "b", "nas:/b")
	one(t, e.manager(), b)
	e.manager().Reconcile(context.Background(), nil)
	if !e.h.mounted(b.Path) {
		t.Error("an in-memory-only manager must not unmount mounts it does not remember")
	}
}

func TestReconcileUnmountBusy(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/a"] = fakeExport{}
	busy := true
	e.busy = func(p string) bool { return busy }
	m := e.manager(func(o *Options) { o.StateFile = e.path("mounts.json") })
	a := e.nfsSpec(1, "a", "nas:/a")
	one(t, m, a)

	// A container uses it: stays mounted, umount is not even attempted.
	m.Reconcile(context.Background(), nil)
	if !e.h.mounted(a.Path) || len(e.h.callsTo("umount")) != 0 {
		t.Fatal("busy storage must stay mounted without calling umount")
	}

	// Not busy for us, but the kernel says the target is busy: stays mounted and recorded.
	busy = false
	e.h.busy[a.Path] = true
	m.Reconcile(context.Background(), nil)
	if !e.h.mounted(a.Path) || len(e.h.callsTo("umount")) != 1 {
		t.Fatalf("umount busy: mounted=%v calls=%q", e.h.mounted(a.Path), e.h.callsTo("umount"))
	}
	if bs, _ := os.ReadFile(e.path("mounts.json")); !strings.Contains(string(bs), a.Path) {
		t.Error("a failed umount must stay recorded so it is retried")
	}

	// Next reconcile retries and succeeds.
	e.h.busy[a.Path] = false
	m.Reconcile(context.Background(), nil)
	if e.h.mounted(a.Path) {
		t.Error("retry must unmount")
	}
}

func TestReconcileNeverUnmountsExistingSharedLocal(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/a"] = fakeExport{}
	m := e.manager(func(o *Options) { o.StateFile = e.path("mounts.json") })
	a := e.nfsSpec(1, "a", "nas:/a")
	one(t, m, a)

	// The administrator now declares the same path as an existing mount: ours no longer.
	a.Existing = true
	wantState(t, one(t, m, a), StateReady, "")
	m.Reconcile(context.Background(), nil)
	if !e.h.mounted(a.Path) || len(e.h.callsTo("umount")) != 0 {
		t.Error("an Existing storage must never be unmounted")
	}

	// shared_dir and local specs disappearing never cause umount.
	d := e.path("d")
	os.MkdirAll(d, 0o755)
	m.Reconcile(context.Background(), []Spec{{ID: 5, Type: TypeSharedDir, Path: d}, {ID: 6, Type: TypeLocal, Path: e.path("l")}})
	m.Reconcile(context.Background(), nil)
	if len(e.h.callsTo("umount")) != 0 {
		t.Error("umount ran")
	}
}

func TestReconcileUnmountSkipsStaleRecords(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/a"] = fakeExport{}
	state := e.path("mounts.json")
	m := e.manager(func(o *Options) { o.StateFile = state })
	a := e.nfsSpec(1, "a", "nas:/a")
	one(t, m, a)

	// Someone unmounted it behind our back and mounted something else over the same path.
	e.h.mu.Lock()
	e.h.mounts = nil
	e.h.mu.Unlock()
	e.h.addForeign(a.Path, "intruder:/x")
	m.Reconcile(context.Background(), nil)
	if len(e.h.callsTo("umount")) != 0 || !e.h.mounted(a.Path) {
		t.Error("a mount with a different source must not be unmounted")
	}
	if bs, _ := os.ReadFile(state); strings.Contains(string(bs), a.Path) {
		t.Errorf("stale record must be forgotten: %s", bs)
	}

	// Record whose mount is simply gone.
	e.h.mu.Lock()
	e.h.mounts = nil
	e.h.writeInfo()
	e.h.mu.Unlock()
	e.h.exports["nas:/a"] = fakeExport{}
	one(t, m, a)
	e.h.mu.Lock()
	e.h.mounts = nil
	e.h.writeInfo()
	e.h.mu.Unlock()
	before := len(e.h.callsTo("umount"))
	m.Reconcile(context.Background(), nil)
	if len(e.h.callsTo("umount")) != before {
		t.Error("nothing to unmount, umount must not run")
	}
}

func TestStateFileLoading(t *testing.T) {
	e := newEnv(t)
	dir := e.path("s")
	os.MkdirAll(dir, 0o755)
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{broken"), 0o644)
	m := e.manager(func(o *Options) { o.StateFile = bad })
	if len(m.mounted) != 0 {
		t.Error("a malformed state file is ignored")
	}
	// An unreadable state file location (a directory) is also survivable.
	m = e.manager(func(o *Options) { o.StateFile = dir })
	m.recordMount(e.path("x"), "a:/b") // save fails (rename onto a directory) but must not panic
}

func TestManagerDo(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	const long = time.Minute
	if err := m.Do(1, "heartbeat", long, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := m.Do(1, "heartbeat", long, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var started atomic.Int32
	stuck := func() error { started.Add(1); <-release; return nil }
	begin := time.Now()
	if err := m.Do(1, "heartbeat", 50*time.Millisecond, stuck); !errors.Is(err, ErrUnresponsive) {
		t.Fatalf("stuck call = %v", err)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("the time limit was not honoured")
	}
	// While it is stuck, further calls of the same op return at once and start nothing.
	for i := 0; i < 20; i++ {
		begin := time.Now()
		if err := m.Do(1, "heartbeat", long, stuck); !errors.Is(err, ErrUnresponsive) {
			t.Fatalf("call during stuck call = %v", err)
		}
		if time.Since(begin) > 200*time.Millisecond {
			t.Fatal("a call on a stuck storage blocked")
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d goroutines started for a stuck op, want 1", n)
	}
	// Another op on the same storage, and the same op on another storage, are not held up.
	if err := m.Do(1, "scan", long, func() error { return nil }); err != nil {
		t.Fatalf("other op = %v", err)
	}
	if err := m.Do(2, "heartbeat", long, func() error { return nil }); err != nil {
		t.Fatalf("other storage = %v", err)
	}
	close(release)
	eventually(t, "recovery", func() bool { return m.Do(1, "heartbeat", long, func() error { return nil }) == nil })
}

func TestDefaults(t *testing.T) {
	m := New(Options{})
	if m.o.CallTimeout != 2*time.Second || m.o.MountTimeout != 30*time.Second || m.o.MountinfoPath != "/proc/self/mountinfo" {
		t.Errorf("defaults = %+v", m.o)
	}
	if m.o.Busy("/x") {
		t.Error("default Busy is never busy")
	}
	_ = m.o.IsRoot()
	out, err := m.o.Runner(context.Background(), "echo", "-n", "a b")
	if err != nil || string(out) != "a b" {
		t.Errorf("default runner = %q, %v", out, err)
	}
	if _, err := m.o.Runner(context.Background(), "/nonexistent/cmd-xyz"); err == nil {
		t.Error("missing command must error")
	}
}

func TestReconcileUnsupportedPlatformStatfs(t *testing.T) {
	e := newEnv(t)
	e.sf.failFor[e.path("l")] = ErrUnsupported
	st := one(t, e.manager(), Spec{ID: 1, Type: TypeLocal, Path: e.path("l")})
	wantState(t, st, StateUnsupported, ReasonOther)
}

func TestReconcileEmptyPathIsPathMissing(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	for _, typ := range []string{TypeLocal, TypeNFS, TypeSharedDir} {
		st := one(t, m, Spec{ID: 1, Name: "x", Type: typ, Export: "nas:/m"})
		wantState(t, st, StateFailed, ReasonPathMissing)
		if st.Writable {
			t.Errorf("%s: %+v", typ, st)
		}
	}
	if r := m.Probe(context.Background(), Spec{Type: TypeLocal}); r.OK || r.Reason != ReasonPathMissing {
		t.Errorf("probe of a local storage without a directory: %+v", r)
	}
}
