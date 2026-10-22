package storagemount

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A mount that dropped leaves an empty directory on the root disk. It must not stay ready, or
// downloads and heartbeats would be written to the local disk.

func TestReconcileExistingNFSWithoutMountFails(t *testing.T) {
	for _, typ := range []string{TypeNFS, TypeNFSCache} {
		e := newEnv(t)
		dir := e.path("nas")
		os.MkdirAll(dir, 0o755)
		sp := Spec{ID: 1, Type: typ, Path: dir, Export: "nas:/models", Existing: true, CachePath: e.path("cache")}
		st := one(t, e.manager(), sp)
		wantState(t, st, StateFailed, ReasonNotMounted)
		if st.Writable || !strings.Contains(st.Message, dir) {
			t.Errorf("%s: status = %+v", typ, st)
		}
		if n := len(e.h.callsTo("mount")); n != 0 {
			t.Errorf("%s: Existing must never mount, mount ran %d times", typ, n)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("%s: nothing may be written to the unmounted directory: %v", typ, ents)
		}
	}
}

func TestReconcileExistingNFSMustBeNFS(t *testing.T) {
	e := newEnv(t)
	dir := e.path("nas")
	os.MkdirAll(dir, 0o755)
	// A tmpfs-like mount at the path is a mount point, but not the NFS export.
	e.h.addForeign(dir, "nas:/models")
	info, _ := os.ReadFile(e.h.infoPath)
	if err := os.WriteFile(e.h.infoPath, []byte(strings.Replace(string(info), "nfs4 nas:/models", "ext4 /dev/sdb1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	st := one(t, e.manager(), Spec{ID: 1, Type: TypeNFS, Path: dir, Export: "nas:/models", Existing: true})
	wantState(t, st, StateFailed, ReasonNotMounted)
	if !strings.Contains(st.Message, "ext4") {
		t.Errorf("message = %q", st.Message)
	}
}

func TestReconcileExistingNFSMountedIsReady(t *testing.T) {
	e := newEnv(t)
	dir := e.path("nas")
	os.MkdirAll(dir, 0o755)
	e.h.addForeign(dir, "nas:/models")
	st := one(t, e.manager(), Spec{ID: 1, Type: TypeNFS, Path: dir, Export: "nas:/models", Existing: true})
	wantState(t, st, StateReady, "")
	if !st.Writable {
		t.Errorf("status = %+v", st)
	}
}

func TestReconcileSharedDirNeedsAMountOrANetworkFilesystem(t *testing.T) {
	e := newEnv(t)
	m := e.manager()

	plain := e.path("plain")
	os.MkdirAll(plain, 0o755)
	e.sf.onLocalDisk(plain)
	st := one(t, m, Spec{ID: 1, Type: TypeSharedDir, Path: plain})
	wantState(t, st, StateFailed, ReasonNotMounted)
	if st.Writable || st.TotalBytes != 0 {
		t.Errorf("a failed storage reports no capacity or write access: %+v", st)
	}
	if ents, _ := os.ReadDir(plain); len(ents) != 0 {
		t.Errorf("nothing may be written to the local directory: %v", ents)
	}

	// The path is a mount point (even one that is not a network filesystem).
	mounted := e.path("mounted")
	os.MkdirAll(mounted, 0o755)
	e.sf.onLocalDisk(mounted)
	e.h.addForeign(mounted, "nas:/shared")
	wantState(t, one(t, m, Spec{ID: 2, Type: TypeSharedDir, Path: mounted}), StateReady, "")

	// A directory inside a mounted share is not a mount point, but statfs shows the network filesystem.
	inside := e.path("nas/models")
	os.MkdirAll(inside, 0o755)
	wantState(t, one(t, m, Spec{ID: 3, Type: TypeSharedDir, Path: inside}), StateReady, "")
}

func TestReconcileSharedDirUnreadableMountinfoFallsBackToStatfs(t *testing.T) {
	e := newEnv(t)
	dir := e.path("shared")
	os.MkdirAll(dir, 0o755)
	m := e.manager(func(o *Options) { o.MountinfoPath = e.path("no-such-mountinfo") })
	wantState(t, one(t, m, Spec{ID: 1, Type: TypeSharedDir, Path: dir}), StateReady, "")
	e.sf.onLocalDisk(dir)
	wantState(t, one(t, m, Spec{ID: 1, Type: TypeSharedDir, Path: dir}), StateFailed, ReasonNotMounted)
	// An NFS storage cannot be judged without mountinfo.
	nfs := Spec{ID: 2, Type: TypeNFS, Path: dir, Export: "nas:/m", Existing: true}
	wantState(t, one(t, m, nfs), StateFailed, ReasonOther)
}

func TestReconcileSharedDirStuckStatfsIsUnresponsive(t *testing.T) {
	e := newEnv(t)
	dir := e.path("shared")
	os.MkdirAll(dir, 0o755)
	release := e.sf.blockPath(dir)
	defer close(release)
	wantState(t, one(t, e.manager(), Spec{ID: 1, Type: TypeSharedDir, Path: dir}), StateUnresponsive, "")
}

func TestProbeRefusesWhatReconcileWouldRefuse(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	plain := e.path("plain")
	os.MkdirAll(plain, 0o755)
	e.sf.onLocalDisk(plain)
	if r := m.Probe(context.Background(), Spec{Type: TypeSharedDir, Path: plain}); r.OK || r.Reason != ReasonNotMounted {
		t.Errorf("shared_dir on a local directory: %+v", r)
	}
	if r := m.Probe(context.Background(), Spec{Type: TypeNFS, Path: plain, Export: "nas:/x", Existing: true}); r.OK || r.Reason != ReasonNotMounted {
		t.Errorf("existing nfs without a mount: %+v", r)
	}
	// A local storage is a plain directory by definition.
	if r := m.Probe(context.Background(), Spec{Type: TypeLocal, Path: plain}); !r.OK {
		t.Errorf("local: %+v", r)
	}
}
