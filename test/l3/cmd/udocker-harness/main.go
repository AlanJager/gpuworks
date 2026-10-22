package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
	"gpuworks.ai/gpuworks/internal/agent/runveilcli"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

// imageLibraryPath is the fixture image's own `ENV LD_LIBRARY_PATH`; the
// launch environment must extend the runtime's default search path with it.
const (
	imageLibraryPath  = "/usr/local/lib/fixture"
	launchLibraryPath = "/opt/nvidia-shim:/usr/local/nvidia/lib64:/usr/local/nvidia/lib:/usr/local/cuda/lib64:" + imageLibraryPath
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		child()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		// Exec target: report the identity and environment the guest sees.
		cwd, _ := os.Getwd()
		fmt.Printf("probe uid=%d home=%s cwd=%s user=%s\n", os.Getuid(), os.Getenv("HOME"), cwd, os.Getenv("USER"))
		fmt.Printf("probe ld_library_path=%s\n", os.Getenv("LD_LIBRARY_PATH"))
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "exit" {
		// Exec target: exit with the given status.
		code, _ := strconv.Atoi(os.Args[2])
		os.Exit(code)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func child() {
	for path, want := range map[string]string{
		"/usr/bin/python3": "/etc/alternatives/python3",
		"/usr/bin/c++":     "/etc/alternatives/c++",
		"/usr/local/cuda":  "/etc/alternatives/cuda",
	} {
		got, err := os.Readlink(path)
		if err != nil || got != want {
			fmt.Fprintf(os.Stderr, "%s link=%q err=%v want=%q\n", path, got, err, want)
			os.Exit(20)
		}
	}
	fmt.Printf("udocker-child-ld_library_path=%s\n", os.Getenv("LD_LIBRARY_PATH"))
	fmt.Println("udocker-child-ok")
	time.Sleep(30 * time.Second)
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	self, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("read harness executable: %w", err)
	}
	lower, err := ocifixture.BuildLayer([]ocifixture.Entry{
		{Path: "usr", Dir: true},
		{Path: "usr/local", Dir: true},
		{Path: "usr/local/bin", Dir: true},
		{Path: "usr/local/bin/fixture", Body: self, Mode: 0o755},
		{Path: "usr/local/bin/fixture-hard", Hardlink: "usr/local/bin/fixture"},
		{Path: "usr/bin", Dir: true},
		{Path: "etc", Dir: true},
		// Real images ship an account database; with it the guest runs as
		// fake root (-0) exactly as under udocker.
		{Path: "etc/passwd", Body: []byte("root:x:0:0:root:/root:/bin/sh\n"), Mode: 0o644},
		{Path: "root", Dir: true},
		{Path: "etc/alternatives", Dir: true},
		{Path: "etc/alternatives/python3", Symlink: "/usr/local/bin/fixture"},
		{Path: "etc/alternatives/c++", Symlink: "/usr/local/bin/fixture"},
		{Path: "etc/alternatives/cuda", Symlink: "/usr/local"},
		{Path: "usr/bin/python3", Symlink: "/etc/alternatives/python3"},
		{Path: "usr/bin/c++", Symlink: "/etc/alternatives/c++"},
		{Path: "usr/local/cuda", Symlink: "/etc/alternatives/cuda"},
		{Path: "opt/stale", Body: []byte("remove me")},
	})
	if err != nil {
		return err
	}
	upper, err := ocifixture.BuildLayer([]ocifixture.Entry{{Path: "opt/.wh.stale"}, {Path: "etc/harness-release", Body: []byte("ready\n")}})
	if err != nil {
		return err
	}
	registry, err := ocifixture.StartTLSRegistry([][]byte{lower, upper}, []string{"/usr/bin/python3"},
		[]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LD_LIBRARY_PATH=" + imageLibraryPath})
	if err != nil {
		return err
	}
	defer registry.Close()
	if err := os.Setenv("SSL_CERT_FILE", registry.CAPath); err != nil {
		return err
	}
	image := registry.Image

	baseDir, err := os.MkdirTemp("", "gpuworks-udocker-harness-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(baseDir)
	if err := os.Chmod(baseDir, 0o755); err != nil {
		return fmt.Errorf("make harness data directory traversable: %w", err)
	}
	driver := udocker.New(baseDir)
	available, err := driver.Detect(ctx)
	if err != nil || !available {
		return fmt.Errorf("udocker detect available=%t: %w", available, err)
	}
	var pullLines []string
	// A deployment's pull: the rootfs is unpacked while the image downloads.
	if err := driver.PullImage(agentruntime.WithLaunchIntent(ctx), image, func(line string) { pullLines = append(pullLines, line) }); err != nil {
		return fmt.Errorf("native pull: %w", err)
	}
	for _, line := range pullLines {
		if strings.Contains(line, "falling back") {
			return fmt.Errorf("native pull unexpectedly fell back: %s", line)
		}
	}
	inspection, err := driver.InspectImage(ctx, image)
	if err != nil || !strings.HasPrefix(inspection.ID, "sha256:") {
		return fmt.Errorf("immutable image inspection=%+v: %w", inspection, err)
	}

	firstProgress, err := launchAndCheck(ctx, driver, baseDir, image, "first")
	if err != nil {
		return err
	}
	if containsReuse(firstProgress) {
		return fmt.Errorf("first create unexpectedly reported base reuse: %v", firstProgress)
	}
	if !containsLine(firstProgress, "unpacked during the pull") {
		return fmt.Errorf("first create did not use the rootfs prepared during the pull: %v", firstProgress)
	}
	if runveilSelected() {
		if err := checkRunveil(ctx, driver, baseDir, "first"); err != nil {
			return err
		}
		if err := checkRunveilCLI(ctx, driver, image, "first"); err != nil {
			return err
		}
	}
	if err := driver.Stop(ctx, "first"); err != nil {
		return fmt.Errorf("stop first: %w", err)
	}
	if err := removeAndVerify(ctx, driver, baseDir, "first"); err != nil {
		return err
	}

	// The base rootfs cache exists only where a clone is cheap (reflink); it
	// is built in the background after the first create.
	reflink := native.ReflinkSupported(filepath.Join(baseDir, "udocker"))
	fmt.Printf("udocker-harness-reflink=%t\n", reflink)
	if reflink {
		if err := waitForBase(filepath.Join(baseDir, "udocker", "base")); err != nil {
			return err
		}
	}
	secondProgress, err := launchAndCheck(ctx, driver, baseDir, image, "second")
	if err != nil {
		return err
	}
	if containsReuse(secondProgress) != reflink {
		return fmt.Errorf("second create base reuse=%t with reflink=%t: %v", containsReuse(secondProgress), reflink, secondProgress)
	}
	if err := removeAndVerify(ctx, driver, baseDir, "second"); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(baseDir, "udocker", "base")); (err == nil) != reflink {
		return fmt.Errorf("base cache presence does not match reflink=%t after instance cleanup: %v", reflink, err)
	}
	if err := loadArchiveAndPrepare(ctx, driver, baseDir); err != nil {
		return err
	}
	if runveilSelected() {
		fmt.Println("launcher=runveil")
	}
	fmt.Println("udocker-harness-ok")
	return nil
}

// diagnoseResidualProcesses reports anything still holding the container
// directory. If the sweep of that directory races a live writer, `rmdir` fails
// with ENOTEMPTY and _removedir reports "Error: deleting directory" no matter
// who owns what - which is the one explanation that fits a root-owned
// container being undeletable BY ROOT.
func diagnoseResidualProcesses(name string) {
	if out, err := exec.Command("pgrep", "-af", "containers/"+name+"/ROOT").Output(); err == nil && len(out) > 0 {
		fmt.Printf("udocker-harness-residual-processes for %s:\n%s", name, out)
	}
}

func removeAndVerify(ctx context.Context, driver *udocker.Driver, baseDir, name string) error {
	diagnoseResidualProcesses(name)
	// The udocker CLI cannot necessarily delete the container itself (it chmods
	// a directory it may not own before the rmdir that would have succeeded), so
	// the driver's privileged os.RemoveAll is the reclaim path that counts.
	// Remove must therefore report success exactly when the rootfs is gone:
	// propagating the expected CLI failure would report every teardown as failed
	// to the control plane, which is the regression this harness pins down.
	removeErr := driver.Remove(ctx, name)
	if _, err := os.Lstat(filepath.Join(baseDir, "udocker", "containers", name)); !os.IsNotExist(err) {
		return fmt.Errorf("%s rootfs survived removal: remove=%v stat=%v", name, removeErr, err)
	}
	if removeErr != nil {
		return fmt.Errorf("remove %s reported failure although its rootfs was reclaimed: %w", name, removeErr)
	}
	if names, err := driver.ListContainers(ctx); err != nil || slices.Contains(names, name) {
		return fmt.Errorf("removed container %s is still listed: %v err=%v", name, names, err)
	}
	return nil
}

func launchAndCheck(ctx context.Context, driver *udocker.Driver, baseDir, image, name string) ([]string, error) {
	var progress []string
	started := time.Now()
	_, err := driver.RunWithProgress(ctx, agentruntime.WorkloadSpec{
		Name: name, Image: image, Entrypoint: []string{"/usr/bin/python3"}, Cmd: []string{"child"},
	}, func(phase, message string) { progress = append(progress, phase+": "+message) })
	if err != nil {
		return progress, fmt.Errorf("run %s: %w", name, err)
	}
	root := filepath.Join(baseDir, "udocker", "containers", name, "ROOT")
	for path, want := range map[string]string{
		"usr/bin/python3": "/etc/alternatives/python3",
		"usr/bin/c++":     "/etc/alternatives/c++",
		"usr/local/cuda":  "/etc/alternatives/cuda",
	} {
		got, err := os.Readlink(filepath.Join(root, path))
		if err != nil || got != want {
			return progress, fmt.Errorf("%s rootfs link %s=%q err=%v want=%q", name, path, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "opt", "stale")); !os.IsNotExist(err) {
		return progress, fmt.Errorf("%s whiteout target survived: %v", name, err)
	}
	original, err := os.Stat(filepath.Join(root, "usr", "local", "bin", "fixture"))
	if err != nil {
		return progress, err
	}
	linked, err := os.Stat(filepath.Join(root, "usr", "local", "bin", "fixture-hard"))
	if err != nil || !os.SameFile(original, linked) {
		return progress, fmt.Errorf("%s hardlink not preserved: %v", name, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(baseDir, "udocker-state", name+".log"))
		if strings.Contains(string(data), "udocker-child-ok") {
			if !strings.Contains(string(data), "udocker-child-ld_library_path="+launchLibraryPath+"\n") {
				return progress, fmt.Errorf("%s launched without the image library path; logs=%s", name, data)
			}
			fmt.Printf("udocker-harness-timing container=%s run_to_ready_ms=%d\n", name, time.Since(started).Milliseconds())
			status, err := driver.Status(ctx, name)
			if err != nil || !status.Exists || !status.Running {
				return progress, fmt.Errorf("%s status=%+v: %w", name, status, err)
			}
			// Orphan reclamation can only keep a node clean if the container is
			// actually visible in the listing the sweeper reads.
			if names, err := driver.ListContainers(ctx); err != nil || !slices.Contains(names, name) {
				return progress, fmt.Errorf("running container %s is missing from ListContainers: %v err=%v", name, names, err)
			}
			return progress, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	logs, _ := os.ReadFile(filepath.Join(baseDir, "udocker-state", name+".log"))
	return progress, fmt.Errorf("%s did not execute through proot; logs=%s", name, logs)
}

// waitForBase waits for the background build to publish a complete base.
func waitForBase(dir string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*", ".complete"))
		if len(matches) > 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("background base build did not finish under %s", dir)
}

func containsLine(lines []string, text string) bool {
	for _, line := range lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

func containsReuse(lines []string) bool {
	for _, line := range lines {
		if strings.Contains(line, "reused immutable base rootfs") {
			return true
		}
	}
	return false
}

// runveilSelected mirrors the driver: runveil unless the udocker CLI launcher
// is asked for explicitly.
func runveilSelected() bool { return os.Getenv("GPUWORKS_UDOCKER_LAUNCHER") != "udocker" }

// checkRunveil proves the runveil launcher really bypassed the udocker CLI:
// the tracked process is the patched PRoot itself (not runuser/bash/python),
// no udocker CLI bookkeeping was created, and Exec runs its argv verbatim
// with the guest seeing fake root.
func checkRunveil(ctx context.Context, driver *udocker.Driver, baseDir, name string) error {
	pidData, err := os.ReadFile(filepath.Join(baseDir, "udocker-state", name+".pid"))
	if err != nil {
		return fmt.Errorf("runveil pidfile: %w", err)
	}
	cmdline, err := os.ReadFile("/proc/" + strings.TrimSpace(string(pidData)) + "/cmdline")
	if err != nil {
		return fmt.Errorf("runveil process: %w", err)
	}
	argv := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if !strings.Contains(filepath.Base(argv[0]), "proot") || !slices.Contains(argv, "--kill-on-exit") || !slices.Contains(argv, "-0") {
		return fmt.Errorf("runveil tracked process is not a direct proot launch: %q", argv)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "udocker", "containers", name, "execmode")); !os.IsNotExist(err) {
		return fmt.Errorf("runveil left udocker execmode bookkeeping: %v", err)
	}
	out, err := driver.Exec(ctx, name, []string{"/usr/bin/python3", "probe"})
	if err != nil {
		return fmt.Errorf("runveil exec: %w: %s", err, out)
	}
	if !strings.Contains(string(out), "probe uid=0 home=/root cwd=/root user=root") {
		return fmt.Errorf("runveil exec saw unexpected identity: %s", out)
	}
	if !strings.Contains(string(out), "probe ld_library_path="+launchLibraryPath+"\n") {
		return fmt.Errorf("runveil exec lost the image library path: %s", out)
	}
	return nil
}

// checkRunveilCLI drives `gpuworks-agent runveil` against the running
// container: ps and images see it, exec runs a real PRoot command with the
// workload's identity and returns its status, and logs prints its output.
func checkRunveilCLI(ctx context.Context, driver *udocker.Driver, image, name string) error {
	var stdout, stderr bytes.Buffer
	cli := &runveilcli.CLI{Driver: driver, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Now: time.Now, Poll: 50 * time.Millisecond}
	run := func(want int, args ...string) (string, error) {
		stdout.Reset()
		stderr.Reset()
		if code := cli.Execute(ctx, args); code != want {
			return "", fmt.Errorf("runveil %v exited %d, want %d: %s%s", args, code, want, stdout.String(), stderr.String())
		}
		return stdout.String(), nil
	}
	out, err := run(0, "ps")
	if err != nil {
		return err
	}
	if !strings.Contains(out, name) || !strings.Contains(out, "running") {
		return fmt.Errorf("runveil ps does not show %s running:\n%s", name, out)
	}
	if out, err = run(0, "images"); err != nil {
		return err
	}
	repo := native.ParseRef(image).Local
	if !strings.Contains(out, repo) {
		return fmt.Errorf("runveil images does not list %s:\n%s", repo, out)
	}
	if out, err = run(0, "exec", name, "/usr/bin/python3", "probe"); err != nil {
		return err
	}
	if !strings.Contains(out, "probe uid=0 home=/root") {
		return fmt.Errorf("runveil exec saw an unexpected identity: %s", out)
	}
	// The guest's exit status is the command's.
	if _, err = run(3, "exec", name, "/usr/bin/python3", "exit", "3"); err != nil {
		return err
	}
	if out, err = run(0, "logs", "-n", "5", name); err != nil {
		return err
	}
	if !strings.Contains(out, "udocker-child-ok") {
		return fmt.Errorf("runveil logs lacks the container output: %s", out)
	}
	return nil
}

// loadArchiveAndPrepare loads a docker-save archive under a reference other
// than the one baked into it and prepares a container from it, all without
// the udocker CLI being able to help: the native loader registers exactly the
// requested reference and native create builds the rootfs.
func loadArchiveAndPrepare(ctx context.Context, driver *udocker.Driver, baseDir string) error {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"baked/name:other"}, LayerBytes: 1 << 20})
	if err != nil {
		return err
	}
	const image = "registry.example/loaded/fixture:v1"
	if err := driver.LoadImage(ctx, image, bytes.NewReader(archive)); err != nil {
		return fmt.Errorf("native load: %w", err)
	}
	if ok, err := driver.ImageExists(ctx, image); err != nil || !ok {
		return fmt.Errorf("loaded image missing: %v %w", ok, err)
	}
	if err := driver.Prepare(ctx, agentruntime.WorkloadSpec{Name: "loaded", Image: image}); err != nil {
		return fmt.Errorf("prepare from loaded image: %w", err)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "udocker", "containers", "loaded", "ROOT", "etc", "gpuworks-fixture")); err != nil {
		return fmt.Errorf("loaded image rootfs: %w", err)
	}
	if err := driver.Remove(ctx, "loaded"); err != nil {
		return fmt.Errorf("remove loaded container: %w", err)
	}
	if err := driver.RemoveImage(ctx, image); err != nil {
		return fmt.Errorf("remove loaded image: %w", err)
	}
	if ok, _ := driver.ImageExists(ctx, image); ok {
		return fmt.Errorf("removed image still present")
	}
	fmt.Println("udocker-harness-load-ok")
	return nil
}
