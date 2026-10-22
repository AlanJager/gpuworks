package storagemount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/storagerules"
)

// Reconcile makes every spec usable on this node and returns one Status per spec, in order. Specs
// are handled concurrently and every filesystem call is time limited, so one stuck storage cannot
// stall the others. Mounts this agent created for storages that are no longer listed are
// unmounted unless Busy reports a container using them; Existing, shared_dir and local storages are
// never unmounted.
func (m *Manager) Reconcile(ctx context.Context, specs []Spec) []Status {
	m.recMu.Lock()
	defer m.recMu.Unlock()

	out := make([]Status, len(specs))
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = m.reconcileOne(ctx, sp)
		}()
	}
	wg.Wait()

	keep := make(map[string]bool, len(specs))
	for _, sp := range specs {
		p := filepath.Clean(sp.Path)
		keep[p] = true
		if sp.Existing || sp.Type == TypeSharedDir || sp.Type == TypeLocal {
			m.forgetMount(p) // not ours to unmount, whatever an earlier spec said
		}
	}
	m.unmountStale(ctx, keep)
	return out
}

func (m *Manager) reconcileOne(ctx context.Context, sp Spec) Status {
	st := Status{ID: sp.ID, Path: sp.Path}
	// The server always sends explicit directories; this package has no default of its own.
	if strings.TrimSpace(sp.Path) == "" {
		return failWith(st, ReasonPathMissing, "the storage has no directory on this node")
	}
	if sp.Type == TypeNFSCache && strings.TrimSpace(sp.CachePath) == "" {
		return failWith(st, ReasonPathMissing, "the storage has no cache directory on this node")
	}
	if !filepath.IsAbs(sp.Path) {
		return failWith(st, ReasonOther, fmt.Sprintf("path %q must be absolute", sp.Path))
	}
	if err := m.call(ctx, storageKey(sp.ID), func() error {
		if err := checkCanonicalStoragePath(sp.Path); err != nil {
			return err
		}
		if sp.CachePath != "" {
			if err := checkCanonicalStoragePath(sp.CachePath); err != nil {
				return fmt.Errorf("cache directory: %w", err)
			}
		}
		return nil
	}); err != nil {
		return failErr(st, err, "validate storage directory")
	}
	switch {
	case sp.Type == TypeLocal:
		st = m.reconcileLocal(ctx, sp, st)
	case sp.Type == TypeSharedDir || ((sp.Type == TypeNFS || sp.Type == TypeNFSCache) && sp.Existing):
		st = m.reconcileDir(ctx, sp, st)
	case sp.Type == TypeNFS || sp.Type == TypeNFSCache:
		st = m.reconcileNFS(ctx, sp, st)
	default:
		st.State, st.Reason = StateUnsupported, ReasonOther
		st.Message = fmt.Sprintf("storage type %q is not supported by this agent", sp.Type)
		return st
	}
	if st.State == StateReady && sp.Type == TypeNFSCache {
		st = m.reconcileCache(ctx, sp, st)
	}
	return st
}

func (m *Manager) reconcileLocal(ctx context.Context, sp Spec, st Status) Status {
	if err := m.call(ctx, storageKey(sp.ID), func() error { return os.MkdirAll(sp.Path, 0o755) }); err != nil {
		return failErr(st, err, "create directory")
	}
	return m.finishReady(ctx, sp, st, sp.Path)
}

func (m *Manager) reconcileDir(ctx context.Context, sp Spec, st Status) Status {
	err := m.call(ctx, storageKey(sp.ID), func() error { return checkReadableDir(sp.Path) })
	if err != nil {
		return failErr(st, err, "check directory")
	}
	reason, msg, err := m.checkBacking(ctx, sp, storageKey(sp.ID))
	if err != nil {
		return failErr(st, err, "check mount")
	}
	if reason != "" {
		return failWith(st, reason, msg)
	}
	return m.finishReady(ctx, sp, st, filepath.Join(sp.Path, MetaDirName))
}

func (m *Manager) reconcileNFS(ctx context.Context, sp Spec, st Status) Status {
	if err := storagerules.ValidateExport(sp.Export); err != nil {
		return failWith(st, ReasonOther, err.Error())
	}
	entries, err := m.readMounts()
	if err != nil {
		return failWith(st, ReasonOther, "cannot read mountinfo: "+err.Error())
	}
	if e, ok := FindMount(entries, sp.Path); ok {
		if !strings.HasPrefix(e.FSType, "nfs") || normalizeSource(e.Source) != normalizeSource(sp.Export) {
			return failWith(st, ReasonSourceMismatch,
				fmt.Sprintf("%s is already mounted from %s (%s), expected %s", sp.Path, e.Source, e.FSType, sp.Export))
		}
		return m.finishReady(ctx, sp, st, filepath.Join(sp.Path, MetaDirName))
	}
	if !m.o.IsRoot() {
		st.State, st.Reason = StateUnsupported, ReasonNotRoot
		st.Message = "the agent is not running as root and cannot mount NFS; mount the export yourself and use it as an existing shared directory"
		return st
	}
	opts, err := effectiveOptions(sp.MountOptions)
	if err != nil {
		return failWith(st, ReasonOther, err.Error())
	}
	st.State, st.Message = StateMounting, ""
	if err := m.call(ctx, storageKey(sp.ID), func() error { return os.MkdirAll(sp.Path, 0o755) }); err != nil {
		return failErr(st, err, "create mount point")
	}
	res, err := m.runMount(ctx, storageKey(sp.ID), sp.Export, sp.Path, opts, true)
	if err != nil {
		st = failErr(st, err, "mount")
		if errors.Is(err, ErrUnresponsive) {
			st.Reason = ReasonUnreachable
			st.Message = fmt.Sprintf("the NFS server did not answer within %s", m.o.MountTimeout)
		}
		return st
	}
	if res.err != nil || res.timedOut {
		reason, msg := mountFailure(res, m.o.MountTimeout)
		return failWith(st, reason, msg)
	}
	m.o.Logger.Info("storagemount: mounted", "storage", sp.Name, "export", sp.Export, "path", sp.Path, "options", opts)
	return m.finishReady(ctx, sp, st, filepath.Join(sp.Path, MetaDirName))
}

// finishReady fills capacity and writability once the storage path is usable. A read-only or
// unwritable storage is still ready.
func (m *Manager) finishReady(ctx context.Context, sp Spec, st Status, writeDir string) Status {
	key := storageKey(sp.ID)
	u, err := m.statfs(ctx, key, sp.Path)
	if err != nil {
		return failErr(st, err, "statfs")
	}
	st.State, st.Reason, st.Message = StateReady, "", ""
	st.TotalBytes, st.AvailableBytes = u.TotalBytes, u.AvailableBytes
	w, err := m.writable(ctx, key, writeDir, writeDir != sp.Path)
	if err != nil {
		return failErr(st, err, "write probe")
	}
	st.Writable = w
	return st
}

func (m *Manager) reconcileCache(ctx context.Context, sp Spec, st Status) Status {
	key := cacheKey(sp.ID)
	if err := m.call(ctx, key, func() error { return os.MkdirAll(sp.CachePath, 0o755) }); err != nil {
		st = failErr(st, err, "create cache directory")
		return st
	}
	u, err := m.statfs(ctx, key, sp.CachePath)
	if err != nil {
		return failErr(st, err, "cache statfs")
	}
	st.CachePath = sp.CachePath
	st.CacheTotalBytes, st.CacheAvailableBytes = u.TotalBytes, u.AvailableBytes
	return st
}

func (m *Manager) statfs(ctx context.Context, key, path string) (Usage, error) {
	var u Usage
	err := m.call(ctx, key, func() (err error) { u, err = m.o.Statfs(path); return })
	if err != nil {
		return Usage{}, err // u may still be written by a stuck call
	}
	return u, nil
}

// writable reports whether a temporary file can be created in dir (created first when mkdir is
// set). Failing to write is not an error; only a time out is.
func (m *Manager) writable(ctx context.Context, key, dir string, mkdir bool) (bool, error) {
	ok := false
	err := m.call(ctx, key, func() error {
		ok = writeProbe(dir, mkdir) == nil
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

func writeProbe(dir string, mkdir bool) error {
	if mkdir {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	cerr := f.Close()
	rerr := os.Remove(name)
	return errors.Join(cerr, rerr)
}

func checkReadableDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory: %w", path, fs.ErrNotExist)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func failWith(st Status, reason, msg string) Status {
	st.State, st.Reason, st.Message = StateFailed, reason, msg
	st.Writable = false
	return st
}

// failErr maps a filesystem error to a status: a time out becomes unresponsive, others failed.
func failErr(st Status, err error, what string) Status {
	switch {
	case errors.Is(err, ErrUnresponsive):
		st.State, st.Reason = StateUnresponsive, ""
		st.Message = what + ": filesystem did not respond"
		st.Writable = false
		return st
	case errors.Is(err, ErrUnsupported):
		st.State, st.Reason, st.Message = StateUnsupported, ReasonOther, what+": "+err.Error()
		st.Writable = false
		return st
	case errors.Is(err, fs.ErrPermission):
		return failWith(st, ReasonPermission, what+": "+err.Error())
	case errors.Is(err, fs.ErrNotExist):
		return failWith(st, ReasonPathMissing, what+": "+err.Error())
	}
	return failWith(st, ReasonOther, what+": "+err.Error())
}
