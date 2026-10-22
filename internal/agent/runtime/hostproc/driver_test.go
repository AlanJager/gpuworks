package hostproc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
	oci "gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// toolScript is the package's engine: it records what it was started with,
// then serves until stopped (or exits with $EXIT when set).
const toolScript = `#!/bin/sh
if [ -n "$PIDOUT" ]; then echo $$ > "$PIDOUT"; fi
{
  echo "argv=$*"
  echo "pwd=$(pwd)"
  echo "HOME=$HOME"
  echo "TMPDIR=$TMPDIR"
  echo "XDG_CACHE_HOME=$XDG_CACHE_HOME"
  echo "PKG_PYTHON=$PKG_PYTHON"
  echo "MODEL=$MODEL"
  echo "PATH=$PATH"
  echo "DYLD=$DYLD_LIBRARY_PATH"
  echo "python=$(readlink "$(dirname "$0")/python")"
} > "$OUT"
echo engine started
if [ -n "$EXIT" ]; then exit "$EXIT"; fi
exec sleep 30
`

type tarEntry struct {
	name, body, link string
	mode             int64
	dir              bool
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// envPackage builds a docker-save archive of an environment package laid out
// like the real ones: a base interpreter under /python, a venv under /venv
// whose bin/python is an absolute symlink and whose pyvenv.cfg names /python.
func envPackage(t *testing.T, tag string) []byte {
	t.Helper()
	entries := []tarEntry{
		{name: "python/", dir: true}, {name: "python/bin/", dir: true},
		{name: "python/bin/py", body: "#!/bin/sh\necho base python\n", mode: 0o755},
		{name: "venv/", dir: true}, {name: "venv/bin/", dir: true},
		{name: "venv/bin/tool", body: toolScript, mode: 0o755},
		{name: "venv/bin/python", link: "/python/bin/py"},
		{name: "venv/pyvenv.cfg", body: "home = /python/bin\ninclude-system-site-packages = false\n", mode: 0o644},
	}
	var layer bytes.Buffer
	w := tar.NewWriter(&layer)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, ModTime: time.Unix(0, 0)}
		switch {
		case e.dir:
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case e.link != "":
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, e.link, 0o777
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(e.body))
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(w, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{
		"architecture": "arm64", "os": "darwin",
		"config": map[string]any{
			"Entrypoint": []string{"tool"},
			"Env":        []string{"PATH=/venv/bin:/python/bin:/usr/local/bin:/usr/bin:/bin", "PKG_PYTHON=/python/bin/py"},
			"WorkingDir": "/venv",
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + sha(layer.Bytes())}},
	})
	layerID := strings.Repeat("a", 64)
	manifest, _ := json.Marshal([]map[string]any{{"Config": sha(config) + ".json", "RepoTags": []string{tag}, "Layers": []string{layerID + "/layer.tar"}}})
	var out bytes.Buffer
	aw := tar.NewWriter(&out)
	for _, m := range []struct {
		name string
		data []byte
	}{
		{sha(config) + ".json", config},
		{layerID + "/layer.tar", layer.Bytes()},
		{"manifest.json", manifest},
	} {
		if err := aw.WriteHeader(&tar.Header{Name: m.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(m.data)), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := aw.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func newTestDriver(t *testing.T, image string) *Driver {
	t.Helper()
	d := New(t.TempDir())
	d.caffeinate = ""
	d.tmpRoot = t.TempDir()
	d.getenv = func(key string) string {
		return map[string]string{
			"USER": "tester", "PATH": "/opt/homebrew/bin:/usr/bin", "DYLD_LIBRARY_PATH": "/opt/homebrew/lib", "SHELL": "/bin/sh",
		}[key]
	}
	if err := d.oci.Load(context.Background(), image, bytes.NewReader(envPackage(t, image))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.oci.WaitBackground)
	return d
}

func readKV(t *testing.T, path string) map[string]string {
	t.Helper()
	var data []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte("python=")) {
			data = b
			break
		}
	}
	if data == nil {
		t.Fatalf("%s was never written", path)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		kv[k] = v
	}
	return kv
}

func TestDriverRunsAWorkloadFromAnEnvironmentPackage(t *testing.T) {
	const image = "gpuworks/env-test:1"
	d := newTestDriver(t, image)
	ctx := context.Background()
	if ok, _ := d.ImageExists(ctx, image); !ok {
		t.Fatal("ImageExists = false after load")
	}
	model := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	spec := runtime.WorkloadSpec{
		Name:   "gpuworks-inst-1",
		Image:  image,
		Cmd:    []string{"--model", "/model", "--chat-template=/model/tpl.jinja"},
		Env:    map[string]string{"OUT": out, "MODEL": "/model", "DYLD_LIBRARY_PATH": "/usr/lib/cuda"},
		Mounts: []runtime.Mount{{Source: model, Target: "/model", ReadOnly: true}},
	}
	var phases []string
	id, err := d.RunWithProgress(ctx, spec, func(phase, _ string) { phases = append(phases, phase) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), id) })
	if id != spec.Name || len(phases) < 2 || phases[len(phases)-1] != "container_launch" {
		t.Fatalf("id=%q phases=%v", id, phases)
	}

	got := readKV(t, out)
	envRoot := filepath.Join(d.oci.Layout.ContainersDir(), mustEnvName(t, d, image), "ROOT")
	want := map[string]string{
		"argv":       "--model " + model + " --chat-template=" + model + "/tpl.jinja",
		"pwd":        realPath(t, filepath.Join(envRoot, "venv")),
		"HOME":       filepath.Join(d.instancesDir, id, "home"),
		"TMPDIR":     d.instanceTmp(id),
		"PKG_PYTHON": filepath.Join(envRoot, "python/bin/py"),
		"MODEL":      model,
		"PATH":       filepath.Join(envRoot, "venv/bin") + ":" + filepath.Join(envRoot, "python/bin") + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"DYLD":       "",
		// The package's absolute symlink now points into the package.
		"python": filepath.Join(envRoot, "python/bin/py"),
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
	if !strings.HasPrefix(got["XDG_CACHE_HOME"], d.cacheDir) {
		t.Errorf("XDG_CACHE_HOME = %q, want under %s", got["XDG_CACHE_HOME"], d.cacheDir)
	}
	cfg, err := os.ReadFile(filepath.Join(envRoot, "venv", "pyvenv.cfg"))
	if err != nil || !strings.Contains(string(cfg), "home = "+filepath.Join(envRoot, "python/bin")+"\n") {
		t.Fatalf("pyvenv.cfg not re-rooted: %q %v", cfg, err)
	}

	st, err := d.Status(ctx, id)
	if err != nil || !st.Exists || !st.Running {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	if names, _ := d.RunningContainers(ctx); !slices.Equal(names, []string{id}) {
		t.Fatalf("RunningContainers = %v", names)
	}
	if names, _ := d.ListContainers(ctx); !slices.Equal(names, []string{id}) {
		t.Fatalf("ListContainers = %v", names)
	}
	if err := d.RemoveImage(ctx, image); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("RemoveImage while running = %v", err)
	}
	logs, err := d.Logs(ctx, id, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(logs)
	logs.Close()
	if !strings.Contains(string(text), "engine started") {
		t.Fatalf("logs = %q", text)
	}

	// Exec and the terminal see the workload's environment, not the agent's.
	execOut, err := d.Exec(ctx, id, []string{"sh", "-c", `echo "$HOME|$PKG_PYTHON|$(pwd)|$DYLD_LIBRARY_PATH"`})
	if err != nil {
		t.Fatalf("Exec: %v: %s", err, execOut)
	}
	wantExec := filepath.Join(d.instancesDir, id, "home") + "|" + filepath.Join(envRoot, "python/bin/py") + "|" + realPath(t, filepath.Join(envRoot, "venv")) + "|"
	if strings.TrimSpace(string(execOut)) != wantExec {
		t.Fatalf("Exec output = %q, want %q", execOut, wantExec)
	}
	argv, err := d.ExecPTY(ctx, id, []string{"python"})
	if err != nil {
		t.Fatal(err)
	}
	ptyOut, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil || strings.TrimSpace(string(ptyOut)) != "base python" {
		t.Fatalf("ExecPTY argv %v ran to %q, %v", argv, ptyOut, err)
	}
	// With no command the terminal opens an interactive shell, not a login
	// one: macOS's login profile reorders PATH and hides the package's tools.
	shellArgv, err := d.ExecPTY(ctx, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(shellArgv); n < 2 || shellArgv[n-1] != "-i" || slices.Contains(shellArgv, "-l") {
		t.Fatalf("default terminal argv = %v, want an interactive shell", shellArgv)
	}

	if err := d.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	st = waitStopped(t, d, id)
	if st.ExitCode != 143 {
		t.Fatalf("stopped Status = %+v, want SIGTERM's 143", st)
	}
	if err := d.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if names, _ := d.ListContainers(ctx); len(names) != 0 {
		t.Fatalf("ListContainers after Remove = %v", names)
	}
	for _, dir := range []string{filepath.Join(d.instancesDir, id), d.instanceTmp(id)} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s survived Remove: %v", dir, err)
		}
	}
	if _, err := os.Stat(envRoot); err != nil {
		t.Fatalf("Remove deleted the shared environment: %v", err)
	}
	if err := d.RemoveImage(ctx, image); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Dir(envRoot), filepath.Join(d.cacheDir, filepath.Base(filepath.Dir(envRoot)))} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s survived RemoveImage: %v", dir, err)
		}
	}
	if ok, _ := d.ImageExists(ctx, image); ok {
		t.Fatal("ImageExists after RemoveImage")
	}
}

func TestDriverReportsTheEnginesOwnExitAndSharesOneEnvironment(t *testing.T) {
	const image = "gpuworks/env-test:1"
	d := newTestDriver(t, image)
	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		spec := runtime.WorkloadSpec{Name: name, Image: image, Env: map[string]string{"OUT": filepath.Join(t.TempDir(), "out"), "EXIT": "7"}}
		if _, err := d.Run(ctx, spec); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Remove(context.Background(), name) })
		if st := waitStopped(t, d, name); !st.Exists || st.ExitCode != 7 {
			t.Fatalf("%s Status = %+v, want exit 7", name, st)
		}
	}
	envs, err := os.ReadDir(d.oci.Layout.ContainersDir())
	if err != nil || len(envs) != 1 {
		t.Fatalf("environments = %v, %v; want one shared copy", envs, err)
	}
}

func TestDriverRefusesWhatItCannotRun(t *testing.T) {
	const image = "gpuworks/env-test:1"
	d := newTestDriver(t, image)
	ctx := context.Background()
	if _, err := d.Run(ctx, runtime.WorkloadSpec{Name: "x", Image: "gpuworks/not-pulled:1"}); err == nil || !strings.Contains(err.Error(), "not pulled") {
		t.Fatalf("Run of an absent package = %v", err)
	}
	if _, err := d.Run(ctx, runtime.WorkloadSpec{Name: "x", Image: image, Entrypoint: []string{"no-such-engine"}}); err == nil || !strings.Contains(err.Error(), "not found in the workload PATH") {
		t.Fatalf("Run of a missing executable = %v", err)
	}
	if _, err := d.Run(ctx, runtime.WorkloadSpec{Name: "../escape", Image: image}); err == nil {
		t.Fatal("Run accepted a name with a path in it")
	}
	if _, err := d.Exec(ctx, "never-started", []string{"true"}); err == nil {
		t.Fatal("Exec of an unknown workload succeeded")
	}
	if st, err := d.Status(ctx, "never-started"); err != nil || st.Exists {
		t.Fatalf("Status of an unknown workload = %+v, %v", st, err)
	}
	if err := d.Stop(ctx, "never-started"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Detect(ctx); ok != (d.goos == "darwin") {
		t.Fatalf("Detect = %v on %s", ok, d.goos)
	}
}

// An engine whose pid cannot be recorded is not left running unseen: Run
// kills it and fails, and nothing reports it as a workload.
func TestDriverKillsAnEngineItCannotTrack(t *testing.T) {
	const image = "gpuworks/env-test:1"
	d := newTestDriver(t, image)
	ctx := context.Background()
	// A directory where the pid file goes makes recording it fail.
	if err := os.MkdirAll(d.procs.PIDPath("untracked"), 0o755); err != nil {
		t.Fatal(err)
	}
	pidOut := filepath.Join(t.TempDir(), "pid")
	spec := runtime.WorkloadSpec{Name: "untracked", Image: image, Env: map[string]string{"OUT": filepath.Join(t.TempDir(), "out"), "PIDOUT": pidOut}}
	if _, err := d.Run(ctx, spec); err == nil || !strings.Contains(err.Error(), "record pid") {
		t.Fatalf("Run = %v, want the pid it could not record", err)
	}
	// The engine may have died before it wrote its pid; if it did write it,
	// that process must be gone.
	time.Sleep(300 * time.Millisecond)
	if data, err := os.ReadFile(pidOut); err == nil {
		var pid int
		_, _ = fmt.Sscan(string(data), &pid)
		for deadline := time.Now().Add(5 * time.Second); pid > 0 && procgroup.Alive(pid) && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		if pid > 0 && procgroup.Alive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatalf("engine %d kept running after Run failed", pid)
		}
	}
	if running, _ := d.RunningContainers(ctx); slices.Contains(running, "untracked") {
		t.Fatal("the failed start is listed as running")
	}
}

// An environment whose unpack was interrupted has no ready marker and is
// rebuilt rather than run half-written.
func TestDriverRebuildsAnIncompleteEnvironment(t *testing.T) {
	const image = "gpuworks/env-test:1"
	d := newTestDriver(t, image)
	ctx := context.Background()
	root, err := d.ensureEnv(ctx, image, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(root), readyMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "venv", "bin", "tool")); err != nil {
		t.Fatal(err)
	}
	root2, err := d.ensureEnv(ctx, image, nil)
	if err != nil || root2 != root {
		t.Fatalf("ensureEnv = %q, %v", root2, err)
	}
	if _, err := os.Stat(filepath.Join(root, "venv", "bin", "tool")); err != nil {
		t.Fatalf("incomplete environment was not rebuilt: %v", err)
	}
}

func mustEnvName(t *testing.T, d *Driver, image string) string {
	t.Helper()
	id, err := d.oci.Layout.ImageID(ociRef(image))
	if err != nil {
		t.Fatal(err)
	}
	return envName(id)
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func waitStopped(t *testing.T, d *Driver, id string) runtime.Status {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		st, err := d.Status(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running {
			return st
		}
	}
	t.Fatalf("%s did not stop", id)
	return runtime.Status{}
}

func ociRef(image string) oci.Ref { return oci.ParseRef(image) }

func TestInstanceTmpIsShortAndPrivate(t *testing.T) {
	d := New(t.TempDir())
	// The real root: the path has to leave room for a socket name.
	long := strings.Repeat("gpuworks-deployment-with-a-long-name-", 4)
	if got := d.instanceTmp(long); len(got)+len("/e0324d25-b86c-4ec7-b050-835ee13468c6") > 103 {
		t.Fatalf("instance TMPDIR %q leaves no room for a socket path", got)
	}
	d.tmpRoot = t.TempDir()
	dir, err := d.prepareInstanceTmp("w")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, d.tmpParent()} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, %v; want 0700", path, info.Mode(), err)
		}
	}
	// A parent another user could write into is not trusted.
	if err := os.Chmod(d.tmpParent(), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := d.prepareInstanceTmp("w2"); err == nil || !strings.Contains(err.Error(), "not a private directory") {
		t.Fatalf("prepareInstanceTmp under a world-writable parent = %v", err)
	}
	// Nor is a symlink planted in its place.
	other := t.TempDir()
	d.tmpRoot = t.TempDir()
	if err := os.Symlink(other, d.tmpParent()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.prepareInstanceTmp("w3"); err == nil {
		t.Fatal("prepareInstanceTmp followed a planted symlink")
	}
}

func TestDriverLoadsAnArchiveFile(t *testing.T) {
	d := New(t.TempDir())
	archive := filepath.Join(t.TempDir(), "env.tar")
	if err := os.WriteFile(archive, envPackage(t, "gpuworks/env-test:2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.LoadImageFile(context.Background(), "registry.example/gpuworks/env-test:2", archive); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.ImageExists(context.Background(), "registry.example/gpuworks/env-test:2"); !ok {
		t.Fatal("the archive was not registered under the requested reference")
	}
	if err := d.LoadImageFile(context.Background(), "x:1", filepath.Join(t.TempDir(), "missing.tar")); err == nil {
		t.Fatal("a missing archive loaded")
	}
}

// Node-to-node image sync: what one Mac saves, another loads as the same
// package (same manifest digest).
func TestDriverSavesAPackageAnotherNodeLoads(t *testing.T) {
	const ref = "gpuworks/env-test:3"
	ctx := context.Background()
	source := New(t.TempDir())
	archive := filepath.Join(t.TempDir(), "env.tar")
	if err := os.WriteFile(archive, envPackage(t, ref), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := source.LoadImageFile(ctx, ref, archive); err != nil {
		t.Fatal(err)
	}
	var saved bytes.Buffer
	if err := source.SaveImage(ctx, ref, &saved); err != nil {
		t.Fatal(err)
	}
	target := New(t.TempDir())
	if err := target.LoadImage(ctx, ref, &saved); err != nil {
		t.Fatal(err)
	}
	want, err := source.InspectImage(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	got, err := target.InspectImage(ctx, ref)
	if err != nil || got.ID != want.ID {
		t.Fatalf("loaded identity %+v (%v), saved %+v", got, err, want)
	}
	if err := source.SaveImage(ctx, "gpuworks/absent:1", io.Discard); err == nil || !strings.Contains(err.Error(), "not on this node") {
		t.Fatalf("saving an absent package = %v", err)
	}
}

// A package pulled from a mirror is tagged with the canonical reference and
// shares the mirror's environment.
func TestDriverTagsAPulledPackageUnderItsCanonicalReference(t *testing.T) {
	const mirror, canonical = "registry.example/gpuworks/env-test:1", "gpuworks/env-test:1"
	d := newTestDriver(t, mirror)
	ctx := context.Background()
	if err := d.TagImage(ctx, mirror, canonical); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.ImageExists(ctx, canonical); !ok {
		t.Fatal("canonical reference not registered")
	}
	a, err := d.ensureEnv(ctx, mirror, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.ensureEnv(ctx, canonical, nil)
	if err != nil || a != b {
		t.Fatalf("environments %q and %q (%v), want one", a, b, err)
	}
	inspected, err := d.InspectImage(ctx, canonical)
	if err != nil || !strings.HasPrefix(inspected.ID, "sha256:") || inspected.Env["PKG_PYTHON"] != "/python/bin/py" || len(inspected.Entrypoint) != 1 {
		t.Fatalf("InspectImage = %+v, %v", inspected, err)
	}
	if _, err := d.InspectImage(ctx, "not/pulled:1"); err == nil {
		t.Fatal("inspected an image that is not pulled")
	}
	if err := d.TagImage(ctx, "not/pulled:1", "x:1"); err == nil {
		t.Fatal("tagged an image that is not pulled")
	}
}
