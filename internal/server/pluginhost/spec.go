// Package pluginhost runs plugin processes and speaks gwx/1 with them.
//
// Every plugin is started through the server's own trampoline (package
// pluginexec), which applies the sandbox before executing it. The host
// hands the plugin two socketpairs: on the first the host is the HTTP/2
// client (initialize, health, shutdown, routes, services); on the second
// it serves the host API. The connection is the plugin's identity: nothing
// a plugin sends names its package, so a sibling cannot speak for it.
//
// A supervisor per package keeps a resident plugin running (crash backoff
// from 1 s doubling to 5 min, crashloop after 5 crashes in 10 min with
// retries every 5 min, health probes every 10 s, restart after 3 misses)
// and starts an on-demand plugin when it is called, stopping it when idle.
// Pushed state is not this package's: a StateHost validates and applies
// pushes, and this package only routes them from the identified plugin.
// See docs/dev/extensions.md and ADR 0071.
package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Spec is one package's server part, as the verified install gives it.
type Spec struct {
	ID             string
	Version        string
	ManifestSHA256 extapi.Digest
	// Exe is the plugin binary inside PackageDir; Args follow it ("serve"
	// when empty).
	Exe  string
	Args []string
	// PackageDir is read-execute for the plugin, DataDir (its working and
	// home directory) and TmpDir read-write.
	PackageDir string
	DataDir    string
	TmpDir     string
	// Lifecycle is extapi.LifecycleResident or extapi.LifecycleOnDemand.
	Lifecycle string
	// Grant is what the administrator approved, sent in initialize.
	Grant extapi.PluginGrant
	// Approved maps each capability the package may use (its manifest's
	// requires and uses) to the highest version it may use; the plugin's
	// handshake answer must stay within it and within the offer.
	Approved map[string]int
	// MaxConcurrency bounds server-to-plugin calls in flight (0: 8).
	MaxConcurrency int
	// MemMiB is the cgroup memory limit (0: 128).
	MemMiB int
	// Devices are device nodes the plugin may open.
	Devices []string
	// RequireLandlock refuses to run the plugin without a Landlock
	// sandbox (the certificate's min_isolation).
	RequireLandlock bool
}

// Defaults of a Spec's resource fields (docs/dev/extensions.md).
const (
	DefaultMaxConcurrency = 8
	DefaultMemMiB         = 128
)

func (s *Spec) validate() error {
	switch {
	case !extapi.ValidPackageID(s.ID):
		return fmt.Errorf("pluginhost: %q is not a package id", s.ID)
	case s.Grant.ID != s.ID || s.Grant.Version != s.Version:
		return fmt.Errorf("pluginhost: %s: the grant names %s %s", s.ID, s.Grant.ID, s.Grant.Version)
	case !s.ManifestSHA256.Valid():
		return fmt.Errorf("pluginhost: %s: manifest digest %q", s.ID, s.ManifestSHA256)
	case !filepath.IsAbs(s.Exe) || !filepath.IsAbs(s.PackageDir) || !filepath.IsAbs(s.DataDir) || !filepath.IsAbs(s.TmpDir):
		return fmt.Errorf("pluginhost: %s: the binary and directories must be absolute paths", s.ID)
	case s.Lifecycle != extapi.LifecycleResident && s.Lifecycle != extapi.LifecycleOnDemand:
		return fmt.Errorf("pluginhost: %s: lifecycle %q", s.ID, s.Lifecycle)
	case s.MaxConcurrency < 0 || s.MaxConcurrency > 1024 || s.MemMiB < 0:
		return fmt.Errorf("pluginhost: %s: resource limits out of range", s.ID)
	}
	return nil
}

func (s Spec) clone() Spec {
	s.Args = append([]string(nil), s.Args...)
	s.Devices = append([]string(nil), s.Devices...)
	s.Approved = maps.Clone(s.Approved)
	return s
}

// Timing holds the supervisor's intervals; zero fields take the defaults
// (docs/dev/extensions.md).
type Timing struct {
	InitTimeout     time.Duration // initialize must answer within (10 s)
	HealthEvery     time.Duration // between health probes (10 s)
	HealthTimeout   time.Duration // one probe (3 s)
	HealthMisses    int           // misses in a row before a restart (3)
	BackoffMin      time.Duration // first restart delay (1 s), doubling
	BackoffMax      time.Duration // longest restart delay (5 min)
	HealthyReset    time.Duration // running this long resets the doubling (10 min)
	CrashloopCount  int           // crashes within CrashloopWindow (5)
	CrashloopWindow time.Duration // (10 min)
	CrashloopRetry  time.Duration // retry interval while in crashloop (5 min)
	StopGrace       time.Duration // after shutdown, before SIGTERM (10 s)
	TermGrace       time.Duration // after SIGTERM, before SIGKILL (5 s)
	IdleStop        time.Duration // an on-demand plugin idle this long stops (10 min)
	CallTimeout     time.Duration // a call without a deadline gets this one (60 s)
}

func (t Timing) withDefaults() Timing {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&t.InitTimeout, 10*time.Second)
	def(&t.HealthEvery, 10*time.Second)
	def(&t.HealthTimeout, 3*time.Second)
	def(&t.BackoffMin, time.Second)
	def(&t.BackoffMax, 5*time.Minute)
	def(&t.HealthyReset, 10*time.Minute)
	def(&t.CrashloopWindow, 10*time.Minute)
	def(&t.CrashloopRetry, 5*time.Minute)
	def(&t.StopGrace, 10*time.Second)
	def(&t.TermGrace, 5*time.Second)
	def(&t.IdleStop, 10*time.Minute)
	def(&t.CallTimeout, time.Minute)
	if t.HealthMisses <= 0 {
		t.HealthMisses = 3
	}
	if t.CrashloopCount <= 0 {
		t.CrashloopCount = 5
	}
	return t
}

// Config is what the host needs from the server.
type Config struct {
	// Trampoline is the server binary, started as `<Trampoline>
	// plugin-exec --profile ... -- <exe> serve`; empty is os.Executable.
	Trampoline string
	// Spawner starts plugin processes; nil starts them through
	// Trampoline. Tests inject one.
	Spawner Spawner
	// Core describes this server for initialize: version, install id,
	// platform, HA role and the capabilities it offers.
	Core func() extapi.CoreInfo
	// Clock gives initialize's clocks; nil uses the wall clock for both.
	Clock func() extapi.Clock
	// State applies pushes; nil refuses every push as an unknown route.
	State StateHost
	// Log receives supervision events and the plugins' stderr.
	Log *slog.Logger
	// OnState is called after every change of a plugin's status, outside
	// the host's locks; it must not block.
	OnState func(Status)
	// Environ is the server's environment, filtered for the plugin; nil
	// reads the process environment.
	Environ []string
	// CgroupRoot and CgroupParent place plugins under
	// <root>/<parent>/plugins/<id> when that cgroup is delegated; an empty
	// root leaves plugins in the server's cgroup.
	CgroupRoot   string
	CgroupParent string
	Timing       Timing
}

// StateHost applies pushed state. The owner comes from the connection the
// push arrived on, never from the push.
type StateHost interface {
	// Held returns the tables the server holds for package id, sent in
	// initialize so the plugin can skip pushes it already made.
	Held(id string) []extapi.HeldState
	// Push applies (or refuses) one push. method is PUT or PATCH. A
	// refusal is an *Error, or an *extapi.Error for a body that breaks
	// the wire rules; anything else answers PLUGIN_HOST_FAILED.
	Push(ctx context.Context, owner Owner, method string, push *extapi.StatePush) (*extapi.StateAck, error)
}

// Owner is the package a host API connection belongs to, as accepted at
// its handshake.
type Owner struct {
	ID        string
	Version   string
	Lifecycle string
	Grant     extapi.PluginGrant
	// Use is the negotiated version of each capability the plugin uses.
	Use map[string]int
	// Generation is the process's number (Status.Generation): a push over
	// the connection of a process the host has since replaced carries a
	// lower one.
	Generation uint64
}

// Error is a gwx/1 error: one the host API answers a plugin with, or one a
// plugin answered the host with. Status is the HTTP status.
type Error struct {
	Status int
	Wire   extapi.WireError
}

func (e *Error) Error() string {
	return fmt.Sprintf("gwx %d %s: %s", e.Status, e.Wire.Code, e.Wire.Message)
}

// Refuse builds a host API refusal; its status follows the reason.
func Refuse(reason, code, message string, params map[string]any) *Error {
	return &Error{Status: extapi.StatusForReason(reason),
		Wire: extapi.WireError{Code: code, Message: message, Params: extapi.Params(params), Reason: reason}}
}

// ErrUnknownPlugin is returned for a package id the host does not run.
var ErrUnknownPlugin = errors.New("pluginhost: no such plugin")
