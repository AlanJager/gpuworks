package storagemount

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func probeLeftovers(e *env) []string {
	ents, _ := os.ReadDir(filepath.Join(e.dir, "probe"))
	var names []string
	for _, x := range ents {
		names = append(names, x.Name())
	}
	return names
}

func TestProbeNFSSuccessLeavesNothingBehind(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/models"] = fakeExport{vers: "4.1"}
	m := e.manager()
	spec := Spec{Type: TypeNFS, Path: e.path("never-touched"), Export: "nas:/models", MountOptions: "hard"}
	r := m.Probe(context.Background(), spec)
	if !r.OK || !r.Readable || !r.Writable {
		t.Fatalf("result = %+v", r)
	}
	if r.Version != "4.1" {
		t.Errorf("Version = %q", r.Version)
	}
	for _, want := range []string{"rw", "vers=4.1", "rsize=1048576"} {
		if !strings.Contains(","+r.Options+",", ","+want+",") && !strings.Contains(r.Options, want) {
			t.Errorf("Options %q lacks %q", r.Options, want)
		}
	}
	if r.TotalBytes != 100<<30 || r.AvailableBytes != 40<<30 {
		t.Errorf("capacity = %d/%d", r.TotalBytes, r.AvailableBytes)
	}
	if len(e.h.mountList()) != 0 {
		t.Errorf("probe left mounts behind: %+v", e.h.mountList())
	}
	if l := probeLeftovers(e); len(l) != 0 {
		t.Errorf("probe left directories behind: %v", l)
	}
	if _, err := os.Stat(spec.Path); err == nil {
		t.Error("probe must not touch spec.Path")
	}
	mc, uc := e.h.callsTo("mount"), e.h.callsTo("umount")
	if len(mc) != 1 || len(uc) != 1 || uc[0][1] != mc[0][6] {
		t.Errorf("mount %q umount %q", mc, uc)
	}
	if !strings.HasPrefix(mc[0][6], filepath.Join(e.dir, "probe")) {
		t.Errorf("probe mounted at %s, outside ProbeDir", mc[0][6])
	}
	if mc[0][4] != "rw,hard,noatime" {
		t.Errorf("options = %q", mc[0][4])
	}
}

func TestProbeNFSReadOnly(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/ro"] = fakeExport{readOnly: true}
	r := e.manager().Probe(context.Background(), Spec{Type: TypeNFS, Export: "nas:/ro", MountOptions: "ro"})
	if !r.OK || !r.Readable || r.Writable {
		t.Fatalf("a read-only export is OK but not writable: %+v", r)
	}
	if len(e.h.mountList()) != 0 || len(probeLeftovers(e)) != 0 {
		t.Error("probe left something behind")
	}
}

func TestProbeNFSFailures(t *testing.T) {
	cases := []struct{ output, reason string }{
		{"mount: /mnt/x: wrong fs type, bad option, bad superblock on host:/p, missing codepage or helper program, or other error.", ReasonMissingClient},
		{"mount.nfs: access denied by server while mounting host:/export", ReasonPermission},
		{"mount.nfs: Connection timed out", ReasonUnreachable},
		{"mount.nfs: Failed to resolve server nfs.example: Name or service not known", ReasonUnreachable},
	}
	for _, c := range cases {
		e := newEnv(t)
		e.h.exports["nas:/m"] = fakeExport{failOutput: c.output}
		r := e.manager().Probe(context.Background(), Spec{Type: TypeNFS, Export: "nas:/m"})
		if r.OK || r.Reason != c.reason {
			t.Errorf("%q: result = %+v, want reason %s", c.output, r, c.reason)
		}
		if !strings.Contains(r.Output, c.output) || !strings.Contains(r.Message, c.output) {
			t.Errorf("output must be reported: %+v", r)
		}
		if len(probeLeftovers(e)) != 0 {
			t.Errorf("failed probe left directories: %v", probeLeftovers(e))
		}
		if len(e.h.callsTo("umount")) != 0 {
			t.Error("nothing mounted, nothing to unmount")
		}
	}

	e := newEnv(t)
	r := e.manager().Probe(context.Background(), Spec{Type: TypeNFS, Export: "nas:/does-not-exist"})
	if r.OK || r.Reason != ReasonPathMissing {
		t.Errorf("unknown export: %+v", r)
	}
}

func TestProbeNFSRejectsBeforeMounting(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	for name, sp := range map[string]Spec{
		"bad options":   {Type: TypeNFS, Export: "nas:/m", MountOptions: "rw;x"},
		"empty export":  {Type: TypeNFSCache},
		"option export": {Type: TypeNFS, Export: "-t"},
	} {
		if r := m.Probe(context.Background(), sp); r.OK || r.Reason != ReasonOther || r.Message == "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	e.root = false
	if r := m.Probe(context.Background(), Spec{Type: TypeNFS, Export: "nas:/m"}); r.OK || r.Reason != ReasonNotRoot {
		t.Errorf("not root: %+v", r)
	}
	if r := m.Probe(context.Background(), Spec{Type: "s3"}); r.OK || r.Reason != ReasonOther {
		t.Errorf("unknown type: %+v", r)
	}
	if len(e.h.calls) != 0 {
		t.Errorf("commands ran: %v", e.h.calls)
	}
}

func TestProbeNFSUmountBusyReportsLeftover(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{}
	// Make every umount of a probe mount fail as the kernel would when something holds it open.
	inner := e.h.run
	m := e.manager(func(o *Options) {
		o.Runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "umount" {
				e.h.mu.Lock()
				for _, mt := range e.h.mounts {
					e.h.busy[mt.path] = true
				}
				e.h.mu.Unlock()
			}
			return inner(ctx, name, args...)
		}
	})
	r := m.Probe(context.Background(), Spec{Type: TypeNFS, Export: "nas:/m"})
	if !r.OK {
		t.Fatalf("the probe itself succeeded: %+v", r)
	}
	if !strings.Contains(r.Message, "could not be unmounted") || !strings.Contains(r.Message, "target is busy") {
		t.Errorf("message must tell about the leftover: %q", r.Message)
	}
	if len(probeLeftovers(e)) != 1 {
		t.Errorf("the mount point must be kept while still mounted: %v", probeLeftovers(e))
	}
}

func TestProbeNFSContextCancelledStillCleansUp(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel right after the mount command succeeded.
	inner := e.h.run
	m := e.manager(func(o *Options) {
		o.Runner = func(c context.Context, name string, args ...string) ([]byte, error) {
			out, err := inner(c, name, args...)
			if name == "mount" {
				cancel()
			}
			return out, err
		}
	})
	r := m.Probe(ctx, Spec{Type: TypeNFS, Export: "nas:/m"})
	if r.OK {
		t.Errorf("cancelled probe cannot be OK: %+v", r)
	}
	if len(e.h.mountList()) != 0 || len(probeLeftovers(e)) != 0 {
		t.Errorf("cancelled probe left mounts %v dirs %v", e.h.mountList(), probeLeftovers(e))
	}
}

func TestProbeNFSStuckStatfs(t *testing.T) {
	e := newEnv(t)
	e.h.exports["nas:/m"] = fakeExport{}
	// The probe mount point is random, so block every path under ProbeDir.
	release := make(chan struct{})
	defer close(release)
	m := e.manager(func(o *Options) {
		o.Statfs = func(p string) (Usage, error) {
			if strings.HasPrefix(p, filepath.Join(e.dir, "probe")) {
				<-release
			}
			return Usage{}, nil
		}
	})
	start := time.Now()
	r := m.Probe(context.Background(), Spec{Type: TypeNFS, Export: "nas:/m"})
	if r.OK {
		t.Fatalf("stuck statfs cannot be OK: %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("probe took %s", time.Since(start))
	}
	if r.Version == "" || !r.Readable {
		t.Errorf("what was learned before the stall is kept: %+v", r)
	}
}

func TestProbeDirKinds(t *testing.T) {
	e := newEnv(t)
	m := e.manager()
	shared := e.path("shared")
	os.MkdirAll(shared, 0o755)
	r := m.Probe(context.Background(), Spec{Type: TypeSharedDir, Path: shared})
	if !r.OK || !r.Readable || !r.Writable || r.TotalBytes != 100<<30 {
		t.Errorf("shared_dir: %+v", r)
	}

	r = m.Probe(context.Background(), Spec{Type: TypeSharedDir, Path: e.path("missing")})
	if r.OK || r.Reason != ReasonPathMissing {
		t.Errorf("missing shared_dir: %+v", r)
	}
	if _, err := os.Stat(e.path("missing")); err == nil {
		t.Error("probe must not create directories")
	}

	// A local directory that does not exist yet is judged by its nearest existing parent.
	r = m.Probe(context.Background(), Spec{Type: TypeLocal, Path: e.path("not/yet/there")})
	if !r.OK || !r.Writable {
		t.Errorf("local missing dir: %+v", r)
	}
	if _, err := os.Stat(e.path("not")); err == nil {
		t.Error("probe must not create the local directory")
	}
	if r := m.Probe(context.Background(), Spec{Type: TypeLocal, Path: "rel"}); r.OK {
		t.Error("relative path must fail")
	}

	// Existing NFS mount: probed in place, version taken from mountinfo.
	mnt := e.path("mnt")
	os.MkdirAll(mnt, 0o755)
	e.h.addForeign(mnt, "nas:/x")
	r = m.Probe(context.Background(), Spec{Type: TypeNFS, Path: mnt, Export: "nas:/x", Existing: true})
	if !r.OK || r.Version != "4.2" || !strings.Contains(r.Options, "vers=4.2") {
		t.Errorf("existing: %+v", r)
	}
	if len(e.h.callsTo("mount")) != 0 {
		t.Error("Existing must not mount")
	}
}

func TestProbeStatfsError(t *testing.T) {
	e := newEnv(t)
	d := e.path("d")
	os.MkdirAll(d, 0o755)
	e.sf.failFor[d] = os.ErrPermission
	r := e.manager().Probe(context.Background(), Spec{Type: TypeSharedDir, Path: d})
	if r.OK || r.Reason != ReasonPermission {
		t.Errorf("%+v", r)
	}
}
