package udocker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/runveil"
)

func TestAscendCompatibilityPreservesPreloadsAndContainerLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, image, explicit, want string
		set                         bool
	}{
		{name: "empty", want: ascendCompatibilityTarget},
		{name: "image", image: "/image/a.so /image/b.so", want: ascendCompatibilityTarget + ":/image/a.so /image/b.so"},
		{name: "override", image: "/image/a.so", explicit: "/caller.so", set: true, want: ascendCompatibilityTarget + ":/caller.so"},
		{name: "clear", image: "/image/a.so", set: true, want: ascendCompatibilityTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := runveilDriver(t)
			dir := runveilContainer(t, d, "test")
			config := `{"config":{"Env":["LD_PRELOAD=` + tc.image + `"]}}`
			if err := os.WriteFile(filepath.Join(dir, "container.json"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			if tc.set {
				env["LD_PRELOAD"] = tc.explicit
			}
			mounts := []runveil.Bind{{Host: "/model", Guest: "/model"}}
			got, err := d.ascendCompatibilityBinds("test", env, mounts, []byte("library"))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 || got[0] != mounts[0] || got[1].Guest != "/.dockerenv" || got[2].Guest != ascendCompatibilityTarget {
				t.Fatalf("binds: %+v", got)
			}
			if env["LD_PRELOAD"] != tc.want {
				t.Fatalf("env=%v", env)
			}
			for _, m := range got[1:] {
				info, err := os.Stat(m.Host)
				if err != nil || info.Mode().Perm() != 0644 {
					t.Fatalf("asset mode: %v %v", info, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "ROOT/.dockerenv")); !os.IsNotExist(err) {
				t.Fatalf("image root mutated: %v", err)
			}
			if err := d.Remove(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			for _, m := range got[1:] {
				if _, err := os.Stat(m.Host); !os.IsNotExist(err) {
					t.Fatalf("compatibility file leaked: %v", err)
				}
			}
		})
	}
}

func TestAscendCompatibilityScopeAndErrors(t *testing.T) {
	d := runveilDriver(t)
	runveilContainer(t, d, "test")
	for _, env := range []map[string]string{nil, {"CUDA_VISIBLE_DEVICES": "0"}, {"HYGON_VISIBLE_DEVICES": "0"}} {
		got, err := d.ascendRunveilCompatibility(runtime.WorkloadSpec{Name: "test", Env: env}, map[string]string{}, nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("unrelated workload changed: %v %v", got, err)
		}
	}
	got, err := d.ascendCompatibilityBinds("test", map[string]string{}, nil, nil)
	if err != nil || len(got) != 1 || got[0].Guest != "/.dockerenv" {
		t.Fatalf("marker without qualified library: %v %v", got, err)
	}
	if _, err := d.ascendCompatibilityBinds("missing", map[string]string{}, nil, []byte("so")); err == nil {
		t.Fatal("missing container must fail")
	}
	// A damaged/unwritable destination must not silently drop the required hook.
	file := filepath.Join(d.containerDir("test"), "gpuworks-ascend-compat.so")
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ascendCompatibilityBinds("test", map[string]string{}, nil, []byte("so")); err == nil {
		t.Fatal("library installation failure ignored")
	}
}

func TestCompatibilityFilesConcurrentExecAndUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compat.so")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeCompatibilityFile(path, []byte("old")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := writeCompatibilityFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("upgrade=%q %v", got, err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".ascend-compat-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files leaked: %v %v", files, err)
	}
}

func TestCompatibilityFileReplacesSymlinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	target, path := filepath.Join(dir, "original"), filepath.Join(dir, "compat.so")
	if err := os.WriteFile(target, []byte("same"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := writeCompatibilityFile(path, []byte("same")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
		t.Fatalf("replacement: %v %v", info, err)
	}
	info, err = os.Stat(target)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("symlink target changed: %v %v", info, err)
	}
}

func TestRunveilExecRestoresAscendWorkloadAfterDriverRestart(t *testing.T) {
	d := runveilDriver(t)
	runveilContainer(t, d, "test")
	proot := filepath.Join(t.TempDir(), "proot")
	if err := os.WriteFile(proot, []byte("#!/bin/sh\nprintf '%s\\n' \"$ASCEND_RT_VISIBLE_DEVICES\" \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	d.prootPath = proot
	spec := runtime.WorkloadSpec{Name: "test", Env: map[string]string{"ASCEND_VISIBLE_DEVICES": "7", "ASCEND_RT_VISIBLE_DEVICES": "7"}, Mounts: []runtime.Mount{{Source: t.TempDir(), Target: "/model"}}}
	device := filepath.Join(t.TempDir(), "device")
	if err := os.WriteFile(device, nil, 0600); err != nil {
		t.Fatal(err)
	}
	spec.Mounts = append(spec.Mounts, runtime.Mount{Source: device, Target: "/dev/davinci7"})
	if err := d.rememberSpec("test", spec); err != nil {
		t.Fatal(err)
	}
	d.specs = nil // force restoration from disk, as after agent restart
	out, err := d.Exec(context.Background(), "test", []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"7\n", spec.Mounts[0].Source + ":/model", ":/.dockerenv"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if _, err := d.Exec(context.Background(), "missing", []string{"/bin/sh"}); err == nil {
		t.Fatal("exec without saved spec must fail")
	}
	if err := os.WriteFile(d.SpecPath("test"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	d.specs = nil
	if _, err := d.Exec(context.Background(), "test", []string{"/bin/sh"}); err == nil {
		t.Fatal("corrupt spec must fail")
	}
}

func TestQualifiedTSDUsesContentAndGuestSymlinks(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "usr/local/Ascend/cann-8.5.1/aarch64-linux/lib64/libtsdclient.so")
	if err := os.MkdirAll(filepath.Dir(lib), 0755); err != nil {
		t.Fatal(err)
	}
	data := []byte("verified library")
	if err := os.WriteFile(lib, data, 0644); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	alias := filepath.Join(root, "usr/local/Ascend/ascend-toolkit/latest")
	if err := os.MkdirAll(filepath.Dir(alias), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/Ascend/cann-8.5.1/aarch64-linux", alias); err != nil {
		t.Fatal(err)
	}
	got, err := qualifiedTSDIdentity(root, nil, hash)
	if err != nil || got == "" {
		t.Fatalf("qualified symlink=%q %v", got, err)
	}
	st, err := os.Stat(lib)
	if err != nil {
		t.Fatal(err)
	}
	raw := st.Sys().(*syscall.Stat_t)
	want := fmt.Sprintf("%d:%d:%d:%d", uint64(raw.Dev), raw.Ino, st.Size(), st.ModTime().UnixNano())
	if got != want {
		t.Fatalf("identity=%s want %s", got, want)
	}
	if err := os.WriteFile(lib, []byte("fixed library"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = qualifiedTSDIdentity(root, nil, hash)
	if err != nil || got != "" {
		t.Fatalf("unknown version qualified=%q %v", got, err)
	}
	mounted := filepath.Join(t.TempDir(), "libtsdclient.so")
	if err := os.WriteFile(mounted, data, 0644); err != nil {
		t.Fatal(err)
	}
	got, err = qualifiedTSDIdentity(root, []runveil.Bind{{Host: mounted, Guest: "/usr/local/Ascend/cann-8.5.1/aarch64-linux/lib64/libtsdclient.so"}}, hash)
	if err != nil || got == "" {
		t.Fatalf("bound library not recognized=%q %v", got, err)
	}
	if got, err := qualifiedTSDIdentity(t.TempDir(), nil, hash); err != nil || got != "" {
		t.Fatalf("no CANN=%q %v", got, err)
	}
}
