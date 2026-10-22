// Package storagemount makes storages usable on one agent node and reports their state. It mounts
// NFS exports through an injectable mount command, checks health with time-limited filesystem
// calls (a hard NFS mount can block a syscall forever), and reads and writes the heartbeat files
// that let several control planes see each other on a shared export.
//
// It is a leaf package: standard library (and x/sys) only.
package storagemount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Storage types.
const (
	TypeLocal     = "local"
	TypeNFS       = "nfs"
	TypeNFSCache  = "nfs_cache"
	TypeSharedDir = "shared_dir"
)

// Storage states.
const (
	StateReady        = "ready"
	StateMounting     = "mounting"
	StateFailed       = "failed"
	StateUnresponsive = "unresponsive"
	StateUnsupported  = "unsupported"
)

// Reason codes for failed states and probe results.
const (
	ReasonMissingClient  = "missing_client"
	ReasonPermission     = "permission_denied"
	ReasonUnreachable    = "unreachable"
	ReasonSourceMismatch = "source_mismatch"
	ReasonNotRoot        = "not_root"
	ReasonPathMissing    = "path_missing"
	ReasonNotMounted     = "not_mounted"
	ReasonOther          = "other"
)

// Names inside a storage, below its root. They repeat the shared layout in internal/proto
// (StorageMetaDir and its subdirectories); this package cannot import proto, and a test in
// internal/agent/app keeps the two in step.
const (
	MetaDirName          = ".gpuworks"
	TrashDirName         = "trash"
	ControlPlanesDirName = "control-planes"
)

// ErrUnsupported is returned by the default Statfs on platforms without a real implementation.
var ErrUnsupported = errors.New("storagemount: not supported on this platform")

// Spec is one storage the server wants usable on this node, already merged with node overrides.
type Spec struct {
	ID           uint
	Name, Type   string
	Path         string // local: directory; nfs/nfs_cache: mount point; shared_dir: existing directory
	Export       string // nfs/nfs_cache: "host:/path"
	MountOptions string // comma separated, validated by ValidateOptions
	Existing     bool   // use a mount that already exists; never mount or unmount it
	CachePath    string // nfs_cache: node cache directory
}

// Status is what the agent reports for one storage.
type Status struct {
	ID                         uint
	State, Reason, Message     string
	Path                       string
	Writable                   bool
	TotalBytes, AvailableBytes int64 // filesystem holding Path
	CachePath                  string
	CacheTotalBytes            int64
	CacheAvailableBytes        int64
}

// Runner runs an external command without a shell and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (combinedOutput []byte, err error)

// Usage is a statfs result.
type Usage struct {
	TotalBytes, AvailableBytes int64
	FSType                     uint64 // filesystem magic number, see IsNetworkFS
}

// Options configures a Manager. Zero values select the defaults noted per field.
type Options struct {
	Runner        Runner                           // default: exec.CommandContext(...).CombinedOutput()
	MountinfoPath string                           // default "/proc/self/mountinfo"
	Statfs        func(path string) (Usage, error) // default: real statfs on linux and macOS, ErrUnsupported elsewhere
	IsRoot        func() bool                      // default: os.Geteuid() == 0
	CallTimeout   time.Duration                    // stat, statfs, readdir and write probes; default 2s
	MountTimeout  time.Duration                    // mount and umount commands; default 30s
	StateFile     string                           // JSON file of mount points this agent mounted; "" keeps them in memory only
	ProbeDir      string                           // parent of temporary probe mount points; default os.TempDir()
	Busy          func(path string) bool           // true when a platform container uses something under path; default never
	Logger        *slog.Logger
}

// Manager reconciles storages on this node. It is safe for concurrent use.
type Manager struct {
	o Options
	g *guard

	recMu sync.Mutex // serializes Reconcile

	mu      sync.Mutex
	mounted map[string]mountRecord // mount points this agent mounted, by path
}

// New returns a Manager and loads the persisted mount set from Options.StateFile, if any.
func New(o Options) *Manager {
	if o.Runner == nil {
		o.Runner = defaultRunner
	}
	if o.MountinfoPath == "" {
		o.MountinfoPath = "/proc/self/mountinfo"
	}
	if o.Statfs == nil {
		o.Statfs = defaultStatfs
	}
	if o.IsRoot == nil {
		o.IsRoot = func() bool { return os.Geteuid() == 0 }
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 2 * time.Second
	}
	if o.MountTimeout <= 0 {
		o.MountTimeout = 30 * time.Second
	}
	if o.ProbeDir == "" {
		o.ProbeDir = os.TempDir()
	}
	if o.Busy == nil {
		o.Busy = func(string) bool { return false }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	m := &Manager{o: o, g: newGuard(), mounted: map[string]mountRecord{}}
	m.loadState()
	return m
}

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Do runs fn, a filesystem access under a storage's path, with a time limit and at most one call
// in flight per (storage, op). If fn does not return within timeout, or an earlier call of the
// same op on the same storage is still stuck, it returns ErrUnresponsive at once and starts
// nothing: a hung NFS mount costs one blocked goroutine per op instead of one per caller. fn may
// still be running after Do returned, so it must not hand results back through variables the
// caller reads when Do fails. Different ops on one storage do not block each other.
func (m *Manager) Do(id uint, op string, timeout time.Duration, fn func() error) error {
	return m.g.run(context.Background(), opKey(id, op), timeout, fn)
}

// DoAny is Do with up to slots calls of the same op running side by side on one storage: for
// short operations several clients may issue at once (listing a directory, renaming a file), where
// refusing the second caller while the first is merely busy would be wrong, yet a hung mount must
// still cost a bounded number of goroutines.
func (m *Manager) DoAny(id uint, op string, slots int, timeout time.Duration, fn func() error) error {
	if slots < 1 {
		slots = 1
	}
	keys := make([]string, slots)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s#%d", opKey(id, op), i)
	}
	return m.g.runAny(context.Background(), keys, timeout, fn)
}

// Statfs reports the capacity of the filesystem holding path through the manager's statfs (the
// real one unless a test injected another). It is not time limited; callers bound it.
func (m *Manager) Statfs(path string) (Usage, error) { return m.o.Statfs(path) }

func opKey(id uint, op string) string { return fmt.Sprintf("s%d/op/%s", id, op) }

func storageKey(id uint) string { return fmt.Sprintf("s%d", id) }
func cacheKey(id uint) string   { return fmt.Sprintf("s%d/cache", id) }
