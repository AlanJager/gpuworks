package runveil

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Bind is a PRoot -b host:guest binding.
type Bind struct {
	Host  string
	Guest string
}

// Spec describes one workload launch.
type Spec struct {
	// Proot is the patched PRoot executable.
	Proot string
	// ContainerDir is UDOCKER_DIR/containers/<name>; it holds ROOT and
	// container.json.
	ContainerDir string
	// Name is reported to the guest as container_uuid/container_names.
	Name string
	// Env is the caller's environment. It wins over the image Env.
	Env map[string]string
	// Binds are the caller's volumes, in order. Their host paths must exist.
	Binds []Bind
	// Entrypoint, when non-nil, replaces the image entrypoint and suppresses
	// the image Cmd, as `docker run --entrypoint` does.
	Entrypoint []string
	// Cmd replaces the image Cmd.
	Cmd []string
	// Workdir overrides the image WorkingDir; the account home is the last
	// resort, as in udocker.
	Workdir string
	// Kernel is PRoot's -k release. Empty means the host release.
	Kernel string
	// HostEnv is the launching process environment. Only TERM, PATH and
	// PROOT_TMP_DIR survive, udocker's valid_host_env.
	HostEnv []string
	// Identity is the host account the PRoot process will run as.
	Identity Identity
}

// Command is a ready-to-exec PRoot invocation.
type Command struct {
	Argv    []string
	Env     []string
	Account Account
}

// SystemBinds is udocker's sysdirs_list: bound when present on the host.
var SystemBinds = []string{"/dev", "/proc", "/sys", "/etc/resolv.conf", "/etc/host.conf", "/lib/modules"}

// hostEnvAllowlist is udocker's valid_host_env.
var hostEnvAllowlist = map[string]bool{"TERM": true, "PATH": true, "PROOT_TMP_DIR": true}

const (
	rootPath = "/usr/sbin:/sbin:/usr/bin:/bin"
	userPath = "/usr/sbin:/sbin:/usr/local/bin:/usr/bin:/bin"
)

// Prepare validates spec against the container and builds the PRoot command
// `udocker run` would have executed in P1 mode. Its only side effect is the
// passwd/group pair written into the container directory.
func Prepare(spec Spec) (*Command, error) {
	if spec.Proot == "" {
		return nil, fmt.Errorf("proot executable is not configured")
	}
	root, err := filepath.EvalSymlinks(filepath.Join(spec.ContainerDir, "ROOT"))
	if err != nil {
		return nil, fmt.Errorf("container %s has no root filesystem: %w", spec.Name, err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("container %s root filesystem %s is not a directory", spec.Name, root)
	}
	config, err := LoadImageConfig(spec.ContainerDir)
	if err != nil {
		return nil, err
	}
	account, auth, err := PrepareAccount(spec.ContainerDir, root, config.User, spec.Identity)
	if err != nil {
		return nil, err
	}

	binds, err := assembleBinds(spec.Binds, auth)
	if err != nil {
		return nil, err
	}

	// Caller env first, image Env only for keys the caller left unset.
	env := make(map[string]string, len(spec.Env)+len(config.Env)+16)
	for k, v := range spec.Env {
		env[k] = v
	}
	for _, entry := range config.Env {
		if k, v, ok := strings.Cut(entry, "="); ok && k != "" {
			if _, set := env[k]; !set {
				env[k] = v
			}
		}
	}
	if env["PATH"] == "" {
		env["PATH"] = userPath
		if account.Root() {
			env["PATH"] = rootPath
		}
	}

	cwd := spec.Workdir
	if cwd == "" {
		cwd = config.WorkingDir
	}
	if cwd == "" {
		cwd = account.Home
	}
	if host := hostPath(root, binds, cwd); host == "" || !isDir(host) {
		return nil, fmt.Errorf("invalid working directory %q in container %s", cwd, spec.Name)
	}

	var argv []string
	if spec.Entrypoint != nil {
		argv = append(append(argv, spec.Entrypoint...), spec.Cmd...)
	} else {
		argv = append(argv, config.Entrypoint...)
		if len(spec.Cmd) > 0 {
			argv = append(argv, spec.Cmd...)
		} else {
			argv = append(argv, config.Cmd...)
		}
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("container %s has no command: neither the workload nor the image defines one", spec.Name)
	}
	if !findExecutable(root, binds, argv[0], env["PATH"], cwd) {
		return nil, fmt.Errorf("command not found or not executable in container %s: %s", spec.Name, argv[0])
	}

	// udocker's _run_env_set.
	if _, set := env["HOME"]; !set {
		env["HOME"] = account.Home
	}
	env["USER"] = account.User
	env["LOGNAME"] = account.User
	env["USERNAME"] = account.User
	short := prefix(spec.Name, 8)
	if account.Root() {
		env["PS1"] = short + "# "
	} else {
		env["PS1"] = short + `\$ `
	}
	env["SHLVL"] = "0"
	env["container_ruser"] = spec.Identity.Username
	env["container_root"] = root
	env["container_uuid"] = spec.Name
	env["container_names"] = spec.Name
	env["container_execmode"] = "P1"

	kernel := spec.Kernel
	if kernel == "" {
		kernel = HostKernelRelease()
	}
	cmd := []string{spec.Proot, "--kill-on-exit"}
	for _, bind := range binds {
		cmd = append(cmd, "-b", bind.Host+":"+bind.Guest)
	}
	if account.Root() {
		cmd = append(cmd, "-0")
	} else {
		cmd = append(cmd, "-i", account.UID+":"+account.GID)
	}
	cmd = append(cmd, "-k", kernel, "-r", root, "-w", cwd)
	cmd = append(cmd, argv...)
	return &Command{Argv: cmd, Env: processEnv(spec.HostEnv, env), Account: account}, nil
}

// assembleBinds orders bindings as udocker does: caller volumes, then the
// system directories that exist, then the generated account files.
func assembleBinds(caller []Bind, auth AuthFiles) ([]Bind, error) {
	binds := make([]Bind, 0, len(caller)+len(SystemBinds)+2)
	for _, bind := range caller {
		host, guest := filepath.Clean(bind.Host), path.Clean(bind.Guest)
		if !filepath.IsAbs(host) {
			return nil, fmt.Errorf("invalid host volume path: %s", bind.Host)
		}
		if !path.IsAbs(guest) || guest == "/" {
			return nil, fmt.Errorf("invalid container volume path: %s", bind.Guest)
		}
		if _, err := os.Stat(host); err != nil {
			return nil, fmt.Errorf("invalid host volume path: %s: %w", bind.Host, err)
		}
		binds = append(binds, Bind{Host: host, Guest: guest})
	}
	for _, dir := range SystemBinds {
		if _, err := os.Stat(dir); err == nil {
			binds = append(binds, Bind{Host: dir, Guest: dir})
		}
	}
	return append(binds,
		Bind{Host: auth.Passwd, Guest: "/etc/passwd"},
		Bind{Host: auth.Group, Guest: "/etc/group"},
	), nil
}

// processEnv is the PRoot process environment: the allow-listed host
// variables overlaid by the container environment, sorted for stable output.
func processEnv(host []string, container map[string]string) []string {
	merged := map[string]string{}
	for _, entry := range host {
		if k, v, ok := strings.Cut(entry, "="); ok && hostEnvAllowlist[k] {
			merged[k] = v
		}
	}
	for k, v := range container {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// HostKernelRelease is the running kernel's release string (uname -r), which
// udocker passes to PRoot's -k unchanged.
func HostKernelRelease() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "6.1.1" // udocker's own fallback
	}
	return strings.TrimSpace(string(data))
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
