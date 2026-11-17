// Package pluginexec is the trampoline the server starts every plugin
// through: `gpuworks plugin-exec --profile <json> -- <exe> serve`. Running
// as the plugin-to-be, it applies the sandbox the server chose (process
// group, parent-death signal, no_new_privs, resource limits, nice, a cgroup
// move and a Landlock ruleset on Linux) and then executes the plugin, so the
// host enforces the sandbox instead of trusting the plugin to apply it.
// Landlock and no_new_privs are inherited across execve; the plugin cannot
// undo them. See docs/dev/extensions.md and ADR 0071 (decision 6).
//
// The package imports nothing from this repository: the server builds the
// Profile, and the trampoline runs in a fresh process of the same binary.
package pluginexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Command is the server subcommand that runs the trampoline.
const Command = "plugin-exec"

// Exit codes of a trampoline that did not reach the plugin. Any other code
// is the plugin's own.
const (
	// ExitUsage: the server passed a profile or command line the
	// trampoline cannot read, a defect of the server.
	ExitUsage = 64
	// ExitSetupFailed: applying the sandbox failed (a limit, the working
	// directory, the descriptors), so the plugin was not started.
	ExitSetupFailed = 71
	// ExitIsolationUnavailable: the profile requires Landlock and this
	// kernel has none; the plugin was not started.
	ExitIsolationUnavailable = 78
	// ExitExecFailed: executing the plugin binary failed.
	ExitExecFailed = 127
)

// IsolationCode is the product code a trampoline names on stderr when it
// refuses to start a plugin that requires isolation this host lacks.
const IsolationCode = "PLUGIN_ISOLATION_UNAVAILABLE"

// Profile is what the trampoline applies before it executes the plugin.
// It is an interface between the server and its own trampoline (the same
// binary), never a wire format.
type Profile struct {
	// ID is the package id, used in the cgroup path and in log lines.
	ID string `json:"id"`
	// ParentPID is the server's pid: a trampoline whose parent is already
	// another process (the server died before the parent-death signal was
	// armed) exits instead of starting an orphan.
	ParentPID int `json:"parent_pid"`
	// Dir is the working directory, the plugin's data directory.
	Dir string `json:"dir"`
	// Nice is added to the plugin's scheduling priority (0 to 19).
	Nice int `json:"nice"`
	// NoFile is the open-file limit; 0 leaves it.
	NoFile uint64 `json:"nofile"`
	// NProc is the process limit; 0 leaves it. RLIMIT_NPROC counts every
	// task of the user, so it bounds one plugin only when the plugin has
	// a user of its own; under the server's user the cgroup's pids.max is
	// the bound.
	NProc uint64 `json:"nproc,omitempty"`
	// Landlock is the filesystem ruleset; nil applies none.
	Landlock *Landlock `json:"landlock,omitempty"`
	// RequireLandlock refuses to start the plugin where Landlock is not
	// available (ExitIsolationUnavailable) instead of running it without.
	RequireLandlock bool `json:"require_landlock,omitempty"`
	// Cgroup moves the plugin into a cgroup of its own when the server's
	// cgroup is delegated to it; nil does not try.
	Cgroup *Cgroup `json:"cgroup,omitempty"`
}

// Landlock lists the paths the plugin may reach; everything else on the
// filesystem is denied. Paths that do not exist are skipped.
type Landlock struct {
	// ReadExec: read and execute (the package directory).
	ReadExec []string `json:"read_exec,omitempty"`
	// ReadWrite: every filesystem right (the data and tmp directories).
	ReadWrite []string `json:"read_write,omitempty"`
	// Read: read only (CA certificates, resolv.conf, hosts, zoneinfo).
	Read []string `json:"read,omitempty"`
	// Devices: read, write and ioctl on device nodes host setup approved.
	Devices []string `json:"devices,omitempty"`
}

// Cgroup places the plugin at <Root>/<Parent>/plugins/<id>.
type Cgroup struct {
	// Root is the cgroup v2 mount, normally /sys/fs/cgroup.
	Root string `json:"root"`
	// Parent is the server's own cgroup relative to Root, which must be
	// delegated to the server's user (systemd Delegate=yes).
	Parent string `json:"parent"`
	// The limits; 0 leaves the controller's default.
	MemoryMax  int64 `json:"memory_max,omitempty"`
	MemoryHigh int64 `json:"memory_high,omitempty"`
	CPUWeight  int   `json:"cpu_weight,omitempty"`
	PidsMax    int   `json:"pids_max,omitempty"`
}

// Dir is the plugin's cgroup directory.
func (c *Cgroup) Dir(id string) string {
	return filepath.Join(c.Root, c.Parent, "plugins", id)
}

// Encode returns p as the --profile argument.
func (p *Profile) Encode() (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// DecodeProfile reads a --profile argument strictly.
func DecodeProfile(s string) (*Profile, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("plugin-exec: profile: %w", err)
	}
	if dec.More() {
		return nil, errors.New("plugin-exec: profile: trailing data")
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Profile) validate() error {
	switch {
	case p.ID == "" || strings.ContainsAny(p.ID, "/\\.") || len(p.ID) > 64:
		return fmt.Errorf("plugin-exec: profile: id %q is not a package id", p.ID)
	case p.ParentPID < 1:
		return errors.New("plugin-exec: profile: parent_pid is required")
	case !filepath.IsAbs(p.Dir):
		return fmt.Errorf("plugin-exec: profile: dir %q is not absolute", p.Dir)
	case p.Nice < 0 || p.Nice > 19:
		return fmt.Errorf("plugin-exec: profile: nice %d is outside 0..19", p.Nice)
	case p.Cgroup != nil && (!filepath.IsAbs(p.Cgroup.Root) || strings.Contains(p.Cgroup.Parent, "..")):
		return fmt.Errorf("plugin-exec: profile: cgroup %+v is not an absolute root and a relative parent", *p.Cgroup)
	}
	if l := p.Landlock; l != nil {
		for _, list := range [][]string{l.ReadExec, l.ReadWrite, l.Read, l.Devices} {
			for _, path := range list {
				if !filepath.IsAbs(path) {
					return fmt.Errorf("plugin-exec: profile: landlock path %q is not absolute", path)
				}
			}
		}
	}
	return nil
}

// ParseArgs reads the trampoline's command line after the subcommand:
// --profile <json> -- <exe> [args...].
func ParseArgs(args []string) (*Profile, []string, error) {
	if len(args) < 4 || args[0] != "--profile" || args[2] != "--" {
		return nil, nil, errors.New("plugin-exec: usage: plugin-exec --profile <json> -- <exe> [args...]")
	}
	p, err := DecodeProfile(args[1])
	if err != nil {
		return nil, nil, err
	}
	argv := args[3:]
	if !filepath.IsAbs(argv[0]) {
		return nil, nil, fmt.Errorf("plugin-exec: the plugin binary %q is not an absolute path", argv[0])
	}
	return p, argv, nil
}

// Args is the trampoline command line for p and the plugin's argv, after
// the subcommand.
func Args(p *Profile, argv ...string) ([]string, error) {
	enc, err := p.Encode()
	if err != nil {
		return nil, err
	}
	return append([]string{Command, "--profile", enc, "--"}, argv...), nil
}
