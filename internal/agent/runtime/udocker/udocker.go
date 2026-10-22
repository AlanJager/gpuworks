// Package udocker implements runtime.Driver for no-root / no-daemon nodes
// (autodl, compshare containers, shared clusters) using udocker + a patched
// proot. It encodes the pitfalls from the runpod evaluation as a deterministic
// pipeline: non-root user, UDOCKER_DIR on local disk, patched proot (socket-free
// seccomp), P1 exec mode, an nvidia driver shim, host-path loopback symlinks,
// and baked NCCL/loader env. udocker has no daemon, so instances are tracked as
// background process groups with a pidfile + redirected log.
package udocker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Driver runs containers via udocker.
type Driver struct {
	provisionMu        sync.Mutex
	provisioned        bool
	track              provisionTracker
	udockerDir         string // UDOCKER_DIR (forced onto local disk)
	stateDir           string // pidfiles + logs for tracked instances
	runUser            string // non-root user to run udocker as ("" = current user)
	runUserHome        string // home used when invoking udocker through runuser
	runUID             int    // resolved uid for runUser, or the current process
	runGID             int    // resolved gid for runUser, or the current process
	prootPath          string // extracted patched proot
	pyBin              string // python3 binary
	pyPath             string // optional offline udocker python module path
	udockerExe         string // optional offline udocker executable
	udockerScript      string // offline Python entry script, invoked with pyBin
	bundleDir          string // optional offline bundle root
	embeddedBundle     bool   // extract the bundle embedded in the agent into bundleDir
	allowOnlineInstall bool   // explicit escape hatch for pip-based provisioning
	offline            bool   // true when udocker came from a local bundle
	nvidiaSMIPath      string // host utility exposed to engines that probe GPU memory through nvidia-smi
	specMu             sync.Mutex
	specs              map[string]runtime.WorkloadSpec // tracked specs for controlled terminal shells
	hostTmpRoot        string                          // short-path root for guest /tmp binds; "" = os.TempDir()
	nativeOnce         sync.Once
	native             *native.Engine
	runveil            bool       // launch containers with runveil instead of the udocker CLI
	cliMu              sync.Mutex // serializes lazy udocker CLI provisioning under runveil
	procMu             sync.Mutex
	tracker            procgroup.Tracker       // pid and exit files of launched workloads, under stateDir
	freeSpace          func(path string) int64 // free bytes for the run user; nil measures the disk (tests stand in a full one)
}

// Option configures the driver.
type Option func(*Driver)

// New builds a udocker driver rooted at baseDir (its UDOCKER_DIR and state live
// under baseDir).
func New(baseDir string) *Driver {
	bundleDir := os.Getenv("GPUWORKS_UDOCKER_BUNDLE")
	embeddedBundle := false
	if bundleDir == "" {
		bundleDir = filepath.Join(baseDir, "offline", "udocker")
		embeddedBundle = true
	}
	driver := &Driver{
		udockerDir:         filepath.Join(baseDir, "udocker"),
		stateDir:           filepath.Join(baseDir, "udocker-state"),
		pyBin:              "python3",
		bundleDir:          bundleDir,
		embeddedBundle:     embeddedBundle,
		allowOnlineInstall: envTrue("GPUWORKS_ALLOW_ONLINE_UDOCKER_INSTALL"),
		specs:              make(map[string]runtime.WorkloadSpec),
		runUID:             os.Geteuid(),
		runGID:             os.Getegid(),
		runveil:            runveilSelected(),
	}
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			path = resolved
		}
		driver.nvidiaSMIPath = path
	}
	return driver
}

func (d *Driver) Name() string { return "udocker" }

// MountOwner returns the identity that will resolve udocker bind-mount source
// paths. Provisioning chooses between the dedicated user and --allow-root, so
// callers must not guess the username before that decision has completed.
func (d *Driver) MountOwner() (runtime.MountOwner, error) {
	d.provisionMu.Lock()
	defer d.provisionMu.Unlock()
	if !d.provisioned {
		return runtime.MountOwner{}, fmt.Errorf("udocker mount owner is unavailable before provisioning")
	}
	if d.runUser == "" && d.runUID == 0 && d.runGID == 0 && os.Geteuid() != 0 {
		return runtime.MountOwner{UID: os.Geteuid(), GID: os.Getegid()}, nil
	}
	return runtime.MountOwner{UID: d.runUID, GID: d.runGID, Username: d.runUser}, nil
}

var _ runtime.MountOwnerProvider = (*Driver)(nil)
var _ runtime.DeviceAccessDiagnoser = (*Driver)(nil)
var _ runtime.DeviceAccessGranter = (*Driver)(nil)

// Diagnostics reports the selected bundle and entrypoint without provisioning
// or touching the node. It is used by the agent Doctor RPC.
func (d *Driver) Diagnostics() string {
	source := "external bundle"
	if d.embeddedBundle {
		source = "embedded offline bundle " + assets.UdockerBundleVersion
	}
	entry := d.udockerExe
	if entry == "" {
		entry = d.udockerScript
	}
	if entry == "" && d.pyPath != "" {
		entry = "python module at " + d.pyPath
	}
	if entry == "" {
		entry = "not resolved until provisioning"
	}
	return fmt.Sprintf("source:   %s\nbundle:   %s\nentry:    %s\nonline:   %t\nlauncher: %s", source, d.bundleDir, entry, d.allowOnlineInstall, d.launcherName())
}

// engineArchName maps a Go architecture to the arch suffix udocker's own
// englib/proot binaries use (proot-<arch>[-4_8_0]), and reports whether
// gpuworks ships a validated patched proot for that architecture.
func engineArchName(goarch string) (string, bool) {
	switch goarch {
	case "amd64":
		return "x86_64", true
	case "arm64":
		return "arm64", true
	default:
		return "", false
	}
}

// Detect reports viability. runveil needs only Linux on an architecture with
// an embedded patched PRoot. The udocker CLI launcher additionally needs
// Python >= 3.6; some accelerator base images keep their only Python in a
// Conda directory that is not on the non-interactive agent's PATH, so the same
// well-known locations as the SSH precheck are tried.
func (d *Driver) Detect(ctx context.Context) (bool, error) {
	if goruntime.GOOS != "linux" {
		return false, nil
	}
	if _, ok := engineArchName(goruntime.GOARCH); !ok {
		return false, nil
	}
	if d.runveil {
		return true, nil
	}
	return d.detectPython(ctx), nil
}

// detectPython finds a Python able to run the udocker CLI and remembers it.
func (d *Driver) detectPython(ctx context.Context) bool {
	py := findPython3()
	if py == "" {
		return false
	}
	d.pyBin = py
	out, err := exec.CommandContext(ctx, py, "-c", "import sys;print(sys.version_info[0]*100+sys.version_info[1])").Output()
	if err != nil {
		return false
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v >= 306 // udocker supports python >= 3.6
}

func findPython3() string {
	if py, err := exec.LookPath("python3"); err == nil {
		return py
	}
	for _, candidate := range []string{
		"/root/miniconda3/bin/python3",
		"/opt/conda/bin/python3",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// provisionRuntime prepares the runtime: the run user, the managed
// directories and the patched PRoot; the udocker CLI too, but only under the
// udocker CLI launcher. The caller holds provisionMu (ensureProvisioned).
func (d *Driver) provisionRuntime(ctx context.Context) error {
	slog.Info("provisioning udocker runtime", "dir", d.udockerDir, "state_dir", d.stateDir, "bundle", d.bundleDir, "embedded", d.embeddedBundle)
	if err := os.MkdirAll(d.udockerDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		return err
	}
	if err := colonNamesAllowed(d.udockerDir); err != nil {
		return err
	}
	// Some container images expose their only Python under /root/conda with a
	// non-traversable /root; the packaged udocker's explicit --allow-root mode
	// is then the only viable path, which decideRunUser falls back to.
	if err := d.decideRunUser(ctx); err != nil {
		return err
	}

	if err := d.ensureStateTraversal(); err != nil {
		return err
	}
	d.warnUntraversableAncestors()

	if d.runveil {
		// runveil needs nothing but the patched PRoot. The udocker CLI is
		// provisioned on first use only (ensureCLI).
		if err := d.extractProot(); err != nil {
			return err
		}
		if err := d.ensureOwned(ctx, d.udockerDir); err != nil {
			return fmt.Errorf("prepare runtime ownership: %w", err)
		}
		if err := d.ensureOwned(ctx, d.stateDir); err != nil {
			return fmt.Errorf("prepare runtime state ownership: %w", err)
		}
		d.provisioned = true
		return nil
	}
	return d.provisionCLI(ctx)
}

// extractProot places the embedded patched PRoot under both names udocker
// looks for, and records it as the executable every launch uses.
func (d *Driver) extractProot() error {
	archName, _ := engineArchName(goruntime.GOARCH)
	binDir := filepath.Join(d.udockerDir, "bin")
	src, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName))
	if err != nil {
		return fmt.Errorf("extract proot: %w", err)
	}
	// udocker also probes a versioned name.
	if _, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName+"-4_8_0")); err != nil {
		return fmt.Errorf("extract versioned proot: %w", err)
	}
	d.prootPath = src
	return nil
}

// ensureOwned performs the expensive recursive ownership migration only for
// legacy roots. Once the root and bin directory belong to the selected user,
// a small marker makes the decision explicit and subsequent agent restarts do
// not walk every rootfs again.
func (d *Driver) ensureOwned(ctx context.Context, root string) error {
	if d.runUser == "" || root == "" {
		return nil
	}
	marker := filepath.Join(root, ".gpuworks-owner")
	want := fmt.Sprintf("%d:%d\n", d.runUID, d.runGID)
	if data, err := os.ReadFile(marker); err == nil && string(data) == want {
		if owned, _ := pathOwnedBy(root, d.runUID, d.runGID); owned {
			return nil
		}
	}
	for _, target := range []string{root, filepath.Join(root, "bin")} {
		if _, err := os.Lstat(target); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		owned, err := pathOwnedBy(target, d.runUID, d.runGID)
		if err != nil {
			return err
		}
		if !owned {
			slog.Info("runtime command", "runtime", "udocker", "args", []string{"chown", "-R", d.runUser + ":" + d.runUser, target})
			if out, err := exec.CommandContext(ctx, "chown", "-R", d.runUser+":"+d.runUser, target).CombinedOutput(); err != nil {
				return fmt.Errorf("chown -R %s %s: %w: %s", d.runUser, target, err, strings.TrimSpace(string(out)))
			}
		}
	}
	return os.WriteFile(marker, []byte(want), 0o644)
}

func pathOwnedBy(path string, uid, gid int) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return int(stat.Uid) == uid && int(stat.Gid) == gid, nil
}

func shouldUseDedicatedUdockerUser(python string, probeErr error) bool {
	return strings.TrimSpace(python) != "" && probeErr == nil
}

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Prepare creates + sets up the container (P1 + nvidia) and applies rootfs
// fixups. Idempotent.
func (d *Driver) Prepare(ctx context.Context, spec runtime.WorkloadSpec) error {
	return d.prepareWithProgress(ctx, spec, func(string, string) {})
}

// Run starts the container as a tracked background process group.
func (d *Driver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	return d.RunWithProgress(ctx, spec, nil)
}

// RunWithProgress exposes the expensive first-run rootfs preparation stages.
func (d *Driver) RunWithProgress(ctx context.Context, spec runtime.WorkloadSpec, onProgress func(phase, message string)) (string, error) {
	report := func(phase, message string) {
		if onProgress != nil {
			onProgress(phase, message)
		}
	}
	report(proto.PhaseContainerPrepare, "preparing container root filesystem")
	if err := d.prepareWithProgress(ctx, spec, report); err != nil {
		return "", err
	}
	if err := d.checkLaunchSpace(); err != nil {
		return "", err
	}
	report(proto.PhaseContainerLaunch, "launching container process")
	name := spec.Name

	if _, err := d.ensureWorkloadTmp(spec.Name); err != nil {
		slog.Warn("short-path /tmp bind unavailable; guest /tmp stays on the container rootfs", "container", name, "err", err)
	}
	args := d.containerRunArgs(spec)

	// Start detached; capture logs and pgid.
	logPath := d.LogPath(name)
	logf, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	var cmd *exec.Cmd
	if d.runveil {
		command, groups, err := d.runveilCommand(spec, spec.Entrypoint, spec.Cmd, "")
		if err != nil {
			logf.Close()
			return "", err
		}
		args = command.Argv
		cmd = d.runveilExec(nil, command, groups)
	} else {
		full, err := d.workloadInvocation(args, spec)
		if err != nil {
			logf.Close()
			return "", err
		}
		cmd = exec.Command(full[0], full[1:]...)
		// The run-user path embeds these variables in the shell command.  The
		// direct (non-root) path must set them on exec.Cmd as well; otherwise
		// detached workloads look in the default ~/.udocker tree and may select
		// an unpatched proot executable instead of the provisioned local runtime.
		if d.runUser == "" {
			cmd.Env = d.cliEnv()
		}
		cmd.Dir = processDir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group
	}
	slog.Info("runtime command", "runtime", d.launcherName(), "args", args)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		slog.Warn("runtime command failed", "runtime", "udocker", "args", args, "err", err)
		logf.Close()
		return "", err
	}
	if err := d.rememberSpec(name, spec); err != nil {
		slog.Warn("persist udocker workload spec for terminal shell failed", "container", name, "err", err)
	}
	// Reap asynchronously so the process isn't left a zombie.
	if err := d.procs().Track(name, cmd, func() { logf.Close() }); err != nil {
		slog.Warn("record udocker workload pid failed", "container", name, "err", err)
	}
	return name, nil
}

func (d *Driver) prepareWithProgress(ctx context.Context, spec runtime.WorkloadSpec, report func(phase, message string)) error {
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	name := spec.Name
	cliImage, pinned := udockerCLIImage(spec.Image)
	if pinned {
		verified, verifyErr := d.pinnedImageVerified(spec.Image)
		if verifyErr != nil {
			return verifyErr
		}
		if !verified {
			return fmt.Errorf("udocker image %q has not been verified against its pinned digest", spec.Image)
		}
	}
	if d.runveil {
		return d.runveilPrepare(ctx, spec, report)
	}
	created := false
	cleanupPartial := func() {
		if !created {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.udo(cleanupCtx, "rm", name); err != nil {
			slog.Warn("remove partial udocker container after preparation failure", "container", name, "err", err)
		}
	}
	if _, err := d.udo(ctx, "inspect", name); err != nil {
		report(proto.PhaseContainerUnpack, "unpacking image layers into container root filesystem")
		created = true
		if handled, err := d.nativeCreate(ctx, name, spec.Image, func(line string) { report(proto.PhaseContainerUnpack, line) }); handled && err != nil {
			cleanupPartial()
			return err
		} else if handled {
			report(proto.PhaseContainerUnpack, "container root filesystem ready")
		} else if _, err := d.udo(ctx, "create", "--name="+name, cliImage); err != nil {
			// udocker may create the container directory before an individual
			// layer extraction fails. Remove that partial rootfs so controller
			// retries do not progressively exhaust the pod's disk.
			cleanupPartial()
			return fmt.Errorf("udocker create: %w", err)
		}
	}
	if needsNVIDIASetup(spec) {
		report("container_gpu_setup", "configuring NVIDIA runtime support")
		if _, err := d.udo(ctx, "setup", "--nvidia", name); err != nil {
			cleanupPartial()
			return fmt.Errorf("udocker setup --nvidia: %w", err)
		}
	}
	report("container_exec_setup", "configuring PRoot execution mode")
	if _, err := d.udo(ctx, "setup", "--execmode=P1", name); err != nil {
		cleanupPartial()
		return fmt.Errorf("udocker setup P1: %w", err)
	}
	report("container_fixup", "applying GPU runtime compatibility fixes")
	if err := d.rootfsFixupsWithNVIDIA(ctx, name, needsNVIDIASetup(spec)); err != nil {
		cleanupPartial()
		return err
	}
	return nil
}

// cliEnv is the environment contract shared by foreground and detached
// udocker invocations. Keep the caller environment intact so credentials and
// proxy configuration continue to work.
func (d *Driver) cliEnv() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"UDOCKER_DIR="+d.udockerDir,
		"UDOCKER_USE_PROOT_EXECUTABLE="+d.prootPath,
	)
	if d.pyPath != "" {
		env = append(env, "PYTHONPATH="+d.pyPath)
	}
	return env
}

// containerRunArgs assembles the udocker run command. Host network (P1 shares
// host net), GPU selection is passed via CUDA_VISIBLE_DEVICES, and volumes use
// --volume. Unlike Docker, udocker's --rm is a boolean flag and rejects
// --rm=false; omitting it is the supported way to preserve the container.
func (d *Driver) containerRunArgs(spec runtime.WorkloadSpec) []string {
	args := []string{"run"}
	env := d.runEnv(spec)
	for k, v := range env {
		args = append(args, "--env="+k+"="+v)
	}
	for _, m := range d.workloadMounts(spec) {
		// udocker's --volume syntax has no portable read-only qualifier across
		// supported releases. Keep the bind target stable; callers that require
		// strict immutability must enforce it in the guest/runtime-specific path.
		args = append(args, "--volume="+m.Source+":"+m.Target)
	}
	// udocker treats command arguments as arguments to the image metadata
	// entrypoint.  WorkloadSpec.Entrypoint, however, is an explicit override
	// (the same contract used by the Docker driver).  Pass its executable via
	// udocker's --entrypoint option, then append the remaining entrypoint tokens
	// and Cmd as arguments.  Without this, an image whose metadata entrypoint is
	// e.g. ["vllm", "serve"] would receive another "vllm serve" prefix and
	// execute "vllm serve vllm serve ...".
	if len(spec.Entrypoint) > 0 {
		args = append(args, "--entrypoint="+spec.Entrypoint[0])
	}
	args = append(args, spec.Name)
	// The first entrypoint token was supplied as --entrypoint above; the rest
	// are the explicit entrypoint arguments and must precede the workload args.
	if len(spec.Entrypoint) > 1 {
		args = append(args, spec.Entrypoint[1:]...)
	}
	args = append(args, spec.Cmd...)
	return args
}

// workloadMounts is every bind a workload gets: the caller's mounts, the host
// nvidia-smi, the accelerator device nodes, and a short-path /tmp.
func (d *Driver) workloadMounts(spec runtime.WorkloadSpec) []runtime.Mount {
	mounts := append([]runtime.Mount(nil), spec.Mounts...)
	if d.nvidiaSMIPath != "" && !isDomesticSpec(spec) && !hasMountTarget(mounts, "/usr/bin/nvidia-smi") {
		// The NVIDIA setup exposes devices and driver libraries, but not the
		// host utility. SGLang invokes nvidia-smi during argument validation
		// to determine memory capacity, before CUDA initialization starts.
		mounts = append(mounts, runtime.Mount{Source: d.nvidiaSMIPath, Target: "/usr/bin/nvidia-smi", ReadOnly: true})
	}
	mounts = append(mounts, acceleratorMounts(spec)...)
	if !hasMountTarget(mounts, "/tmp") {
		// A caller-provided /tmp keeps winning; see workload_tmp.go for why the
		// guest /tmp needs a short host path at all.
		if source, ok := d.preparedWorkloadTmp(spec.Name); ok {
			mounts = append(mounts, runtime.Mount{Source: source, Target: "/tmp"})
		}
	}
	return mounts
}

func hasMountTarget(mounts []runtime.Mount, target string) bool {
	for _, mount := range mounts {
		if filepath.Clean(mount.Target) == target {
			return true
		}
	}
	return false
}

// ListContainers lists udocker container names, including stopped ones.
//
// udocker has no daemon to query, so the listing is the container directory
// layout under UDOCKER_DIR/containers: one entry per container, named exactly
// as GPUWorks created it (`--name=<gpuworks-id>`, or the generated id for a
// bare `udocker create`). Reading the directory is deliberate — `udocker ps`
// would route the query through runuser/python, and it truncates names, which
// is precisely what an orphan sweep must not do.
//
// Only entries that actually carry a ROOT directory count as containers, and
// the `<name>.creating` staging directory of an in-flight native create is
// excluded explicitly even though it already has a ROOT: reporting it would
// make the orphan sweeper delete a rootfs that is still being built.
func (d *Driver) ListContainers(ctx context.Context) ([]string, error) {
	containersDir := filepath.Join(d.udockerDir, "containers")
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing has been provisioned on this node yet.
			return nil, nil
		}
		return nil, fmt.Errorf("list udocker containers: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, native.CreateStagingSuffix) {
			continue
		}
		// Stat rather than trusting the entry type: a container name is a
		// symlink when udocker created it via --name, and udocker itself treats
		// those aliases as containers.
		root, err := os.Stat(filepath.Join(containersDir, name, "ROOT"))
		if err != nil || !root.IsDir() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// RunningContainers lists the workloads whose tracked process group is alive,
// from the same pid files Status reads; no CLI is involved.
func (d *Driver) RunningContainers(ctx context.Context) ([]string, error) {
	return d.procs().Running()
}

// Status reports whether the tracked process group is alive.
func (d *Driver) Status(ctx context.Context, id string) (runtime.Status, error) {
	exists, running, code := d.procs().Status(ctx, id)
	return runtime.Status{Exists: exists, Running: running, ExitCode: code}, nil
}

// Logs returns the redirected log stream: the last tail lines (all of it when
// tail <= 0), then, when following, whatever the container writes next.
func (d *Driver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	return procgroup.Logs(ctx, d.LogPath(id), follow, tail)
}

// Stop kills the process group.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if pid, ok := d.procs().PID(id); ok {
		slog.Info("stopping udocker workload process group", "container", id, "pid", pid)
	}
	return d.procs().Stop(ctx, id, 3*time.Second)
}

// procs is the tracker of this driver's workloads. Tests point stateDir
// elsewhere after construction, so the tracker follows it.
func (d *Driver) procs() *procgroup.Tracker {
	d.procMu.Lock()
	defer d.procMu.Unlock()
	if d.tracker.Dir != d.stateDir {
		d.tracker.Dir = d.stateDir
	}
	return &d.tracker
}

// Remove stops and deletes the udocker container.
//
// `udocker rm` fails on this deployment shape, and its failure is not an error
// condition this call should propagate - it is the expected outcome, so it
// costs a log line rather than the teardown. What it reports is:
//
//	Info: deleting container: <id>
//	Error: deleting directory:  <UDOCKER_DIR>/containers/<id>
//	Error: deleting container
//
// ROOT CAUSE (established 2026-09-14). The container ROOT holds entries owned
// by root - the agent's own uid - while `udocker rm` runs as the run user
// through runuser. rootfsFixupsWithNVIDIA() (helpers.go) runs IN the agent
// process and writes into the container ROOT *after* the unpack has already
// handed every entry to the run user:
//
//	ROOT/etc/hosts                 atomicWriteFile() = os.CreateTemp + os.Rename
//	ROOT/opt/nvidia-shim/          os.MkdirAll + four copyDriverLib() files
//	ROOT/<UDOCKER_DIR>/…/containers/  ensureHostRootLoopback()'s os.MkdirAll chain
//
// atomicWriteFile never chowns, so those entries stay root-owned. upstream
// FileUtil._removedir() chmods every file before unlinking it, and chmod on a
// root-owned file is EPERM, which its bare `except OSError` swallows into
// `return False`; the CLI then reports a directory it could not delete without
// saying why. ROOT/etc/hosts is written UNCONDITIONALLY, so every container is
// affected, not only the ones that need the NVIDIA shim.
//
// This supersedes two earlier readings of this comment: the first blamed a
// root-owned ROOT and was withdrawn, the second declared the cause unknown. The
// ownership problem is real, but it is INSIDE ROOT rather than at the container
// directory or at ROOT itself - both of which the unpack does hand to the run
// user (measured 2026-09-13: 1002:1002, mode 755) - which is why measuring only
// those two levels kept coming back clean. The failure was reproduced verbatim
// on the validation node by recreating the fixup artifacts and running the real
// CLI: `chmod` on ROOT/etc/hosts fails with EPERM (errno 1), then the walk
// aborts and the CLI prints the same three lines quoted above.
//
// The os.RemoveAll below therefore reclaims the rootfs on this shape because it
// runs as the agent's root rather than through runuser - root needs no
// permission to unlink a file it owns. Its result, not the CLI's, decides this
// call's return value. Fixing the ownership at the source - chown the fixup
// artifacts as they are written, or chown the tree once the fixups are done -
// would let the CLI succeed as well; until then this fallback is load-bearing
// rather than decorative, and it is the reason a workload that never runs as
// root cannot be torn down here.
func (d *Driver) Remove(ctx context.Context, id string) error {
	if err := d.Stop(ctx, id); err != nil {
		return err
	}
	// The failure of this command is expected and explained in the doc comment,
	// so it is logged once for the operator without the generic runtime-command
	// warning that would otherwise mark every single teardown as a problem.
	if d.runveil {
		// runveil owns the container directory outright; there is no CLI
		// bookkeeping to undo.
		if err := d.removeContainerDir(id); err != nil {
			return fmt.Errorf("remove container rootfs %s: %w", id, err)
		}
		d.forgetState(id)
		return nil
	}
	_, rmErr := d.udoExpected(ctx, "rm", id)
	if rmErr != nil {
		slog.Info("udocker rm did not delete the container; falling back to a privileged rootfs removal", "container", id, "err", rmErr)
	}
	// Older udocker releases can also report success while leaving the
	// materialized ROOT behind (and report "not found" when only metadata was
	// lost). Reap the exact managed container directory as a bounded fallback;
	// never broaden the target beyond UDOCKER_DIR/containers/<id>.
	containerDir := filepath.Join(d.udockerDir, "containers", filepath.Base(id))
	if filepath.Base(id) == id {
		d.removeWorkloadTmp(id)
		if info, statErr := os.Lstat(containerDir); statErr == nil {
			slog.Info("removing udocker container rootfs", "container", id, "dir", containerDir)
			if removeErr := os.RemoveAll(containerDir); removeErr != nil {
				return fmt.Errorf("remove udocker container rootfs %s: %w", id, removeErr)
			}
			// A name created by `udocker create --name=` is a symlink to the
			// container's real directory, so this fallback reclaimed nothing and
			// the CLI stays the only signal for the rootfs. Deleting through the
			// link would widen the target beyond this container's directory, so
			// the deletion is reported as failed instead of silently leaking it.
			if info.Mode()&os.ModeSymlink == 0 {
				// The rootfs is gone; the CLI error no longer says anything about
				// the container's actual state.
				rmErr = nil
			}
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect udocker container %s: %w", id, statErr)
		}
	}
	d.forgetState(id)
	if rmErr != nil {
		// udocker reports an error for an already removed container. Treat that
		// case as idempotent, but propagate real cleanup failures so reconcile
		// does not report a healthy deletion when rootfs cleanup failed.
		if !strings.Contains(strings.ToLower(rmErr.Error()), "not found") && !strings.Contains(strings.ToLower(rmErr.Error()), "no such") {
			return fmt.Errorf("remove udocker container %s: %w", id, rmErr)
		}
	}
	return nil
}

// forgetState drops the tracking files of a removed container.
func (d *Driver) forgetState(id string) {
	// The process was stopped before this; Forget lets its reaper write the
	// status first so the file is not left behind for the next container.
	d.procs().Forget(id)
	_ = os.Remove(d.LogPath(id))
	_ = os.Remove(d.SpecPath(id))
	d.specMu.Lock()
	delete(d.specs, id)
	d.specMu.Unlock()
}

// Exec runs a command in the container (fresh proot invocation). Under
// runveil cmd is the complete argv, as with `docker exec`; the udocker CLI
// path appends it to the image entrypoint.
func (d *Driver) Exec(ctx context.Context, id string, cmd []string) ([]byte, error) {
	if d.runveil {
		if len(cmd) == 0 {
			return nil, fmt.Errorf("exec requires a command")
		}
		command, groups, err := d.runveilCommand(runtime.WorkloadSpec{Name: id}, cmd, nil, "")
		if err != nil {
			return nil, err
		}
		return d.runveilExec(ctx, command, groups).CombinedOutput()
	}
	args := append([]string{"run", id}, cmd...)
	out, err := d.udo(ctx, args...)
	return []byte(out), err
}

// ExecPTY opens a controlled interactive shell rooted at the tracked workload
// rootfs. udocker 1.3.x has no `exec` command, so this is necessarily a new
// proot process; it is deliberately not described as attaching to the model
// process. The workload's environment, mounts, GPU selection and / workdir
// are reconstructed from the persisted run spec.
func (d *Driver) ExecPTY(ctx context.Context, id string, cmd []string) ([]string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("container terminal requires a container id")
	}
	if err := d.ensureProvisioned(ctx); err != nil {
		return nil, fmt.Errorf("prepare container terminal: %w", err)
	}
	spec, ok, err := d.loadSpec(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("udocker terminal unavailable: workload spec for %q is missing", id)
	}
	if d.runveil {
		return d.runveilPTYArgv(spec, cmd)
	}
	args := []string{"run"}
	for k, v := range d.runEnv(spec) {
		args = append(args, "--env="+k+"="+v)
	}
	for _, m := range d.workloadMounts(spec) {
		args = append(args, "--volume="+m.Source+":"+m.Target)
	}
	// P1 workloads use / as their working directory. Keep this explicit so a
	// shell opened from a different host cwd still sees the model's workdir.
	args = append(args, "--workdir=/")
	if len(cmd) == 0 {
		args = append(args, "--entrypoint=/bin/sh", id, "-l")
	} else {
		args = append(args, "--entrypoint="+cmd[0], id)
		args = append(args, cmd[1:]...)
	}
	return d.udockerInvocation(args), nil
}

func (d *Driver) rememberSpec(id string, spec runtime.WorkloadSpec) error {
	clone := cloneWorkloadSpec(spec)
	d.specMu.Lock()
	if d.specs == nil {
		d.specs = make(map[string]runtime.WorkloadSpec)
	}
	d.specs[id] = clone
	d.specMu.Unlock()
	data, err := json.Marshal(clone)
	if err != nil {
		return err
	}
	return os.WriteFile(d.SpecPath(id), data, 0o600)
}

func (d *Driver) loadSpec(id string) (runtime.WorkloadSpec, bool, error) {
	d.specMu.Lock()
	spec, ok := d.specs[id]
	d.specMu.Unlock()
	if ok {
		return cloneWorkloadSpec(spec), true, nil
	}
	data, err := os.ReadFile(d.SpecPath(id))
	if os.IsNotExist(err) {
		return runtime.WorkloadSpec{}, false, nil
	}
	if err != nil {
		return runtime.WorkloadSpec{}, false, fmt.Errorf("read udocker workload spec: %w", err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return runtime.WorkloadSpec{}, false, fmt.Errorf("decode udocker workload spec: %w", err)
	}
	return spec, true, nil
}

func cloneWorkloadSpec(spec runtime.WorkloadSpec) runtime.WorkloadSpec {
	clone := spec
	clone.Cmd = append([]string(nil), spec.Cmd...)
	clone.Entrypoint = append([]string(nil), spec.Entrypoint...)
	clone.GPUIndexes = append([]int(nil), spec.GPUIndexes...)
	clone.DockerArgs = append([]string(nil), spec.DockerArgs...)
	clone.PreparationNotes = append([]string(nil), spec.PreparationNotes...)
	clone.Env = make(map[string]string, len(spec.Env))
	for k, v := range spec.Env {
		clone.Env[k] = v
	}
	clone.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	clone.Ports = append([]int(nil), spec.Ports...)
	return clone
}

var _ runtime.Driver = (*Driver)(nil)
var _ runtime.RunProgressDriver = (*Driver)(nil)
var _ runtime.PTYExecer = (*Driver)(nil)
