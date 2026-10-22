package storagemount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/storagerules"
)

// ProbeResult is the outcome of testing a storage definition before it is saved.
type ProbeResult struct {
	OK                         bool
	Reason, Message, Output    string
	Version                    string // NFS version negotiated, from the "vers=" super option
	Options                    string // effective mount options as the kernel reports them
	Readable, Writable         bool
	TotalBytes, AvailableBytes int64
}

// Probe tests a storage definition and leaves nothing mounted behind. NFS types are mounted on a
// temporary mount point (spec.Path is not touched), read, written and unmounted again. Local,
// shared_dir and Existing storages are checked in place without creating anything. A read-only
// storage is OK with Writable false.
func (m *Manager) Probe(ctx context.Context, spec Spec) ProbeResult {
	if spec.Path != "" {
		if err := storagerules.ValidatePath(spec.Path); err != nil {
			return probeFail(ReasonPermission, err.Error(), "")
		}
	}
	if spec.Type == TypeNFS || spec.Type == TypeNFSCache {
		if !spec.Existing {
			return m.probeNFS(ctx, spec)
		}
	}
	switch spec.Type {
	case TypeLocal, TypeSharedDir, TypeNFS, TypeNFSCache:
		return m.probeDir(ctx, spec)
	}
	return ProbeResult{Reason: ReasonOther, Message: "unsupported storage type " + spec.Type}
}

func probeFail(reason, msg, output string) ProbeResult {
	return ProbeResult{Reason: reason, Message: msg, Output: output}
}

// probeDir checks an existing directory. A local directory that does not exist yet is judged by
// its nearest existing ancestor, because it will be created when the storage is used.
func (m *Manager) probeDir(ctx context.Context, spec Spec) ProbeResult {
	if strings.TrimSpace(spec.Path) == "" {
		return probeFail(ReasonPathMissing, "no directory was given", "")
	}
	if !filepath.IsAbs(spec.Path) {
		return probeFail(ReasonOther, "path must be absolute", "")
	}
	key := "p:" + spec.Path
	dir := spec.Path
	if spec.Type == TypeLocal {
		for {
			if _, err := os.Stat(dir); err == nil || dir == filepath.Dir(dir) {
				break
			}
			dir = filepath.Dir(dir)
		}
	}
	err := m.call(ctx, key, func() error {
		// Require a canonical directory so a permitted root cannot be a
		// symlink to credentials or another root outside the server's policy.
		if err := checkCanonicalStoragePath(spec.Path); err != nil {
			return err
		}
		return checkReadableDir(dir)
	})
	if err != nil {
		st := failErr(Status{}, err, "check directory")
		return probeFail(reasonOrOther(st), st.Message, "")
	}
	if spec.Type != TypeLocal {
		reason, msg, err := m.checkBacking(ctx, spec, key)
		if err != nil {
			st := failErr(Status{}, err, "check mount")
			return probeFail(reasonOrOther(st), st.Message, "")
		}
		if reason != "" {
			return probeFail(reason, msg, "")
		}
	}
	res := ProbeResult{Readable: true}
	if e, ok := m.mountAt(spec.Path); ok {
		res.Version = superOption(e.SuperOptions, "vers")
		res.Options = mergeOptions(e.Options, e.SuperOptions)
	}
	return m.probeFinish(ctx, key, dir, filepath.Join(dir, MetaDirName), spec.Type == TypeLocal, res)
}

// probeFinish adds capacity and writability to res and marks it OK.
func (m *Manager) probeFinish(ctx context.Context, key, statPath, writeDir string, writeInPlace bool, res ProbeResult) ProbeResult {
	u, err := m.statfs(ctx, key, statPath)
	if err != nil {
		st := failErr(Status{}, err, "statfs")
		res.Reason, res.Message = reasonOrOther(st), st.Message
		return res
	}
	res.TotalBytes, res.AvailableBytes = u.TotalBytes, u.AvailableBytes
	dir := writeDir
	if writeInPlace {
		dir = statPath
	}
	w, err := m.writable(ctx, key, dir, !writeInPlace)
	if err != nil {
		st := failErr(Status{}, err, "write probe")
		res.Reason, res.Message = reasonOrOther(st), st.Message
		return res
	}
	res.Writable = w
	res.OK = true
	return res
}

func reasonOrOther(st Status) string {
	if st.Reason != "" {
		return st.Reason
	}
	return ReasonOther
}

func (m *Manager) mountAt(path string) (MountEntry, bool) {
	entries, err := m.readMounts()
	if err != nil {
		return MountEntry{}, false
	}
	return FindMount(entries, path)
}

func (m *Manager) probeNFS(ctx context.Context, spec Spec) ProbeResult {
	if err := storagerules.ValidateExport(spec.Export); err != nil {
		return probeFail(ReasonOther, err.Error(), "")
	}
	opts, err := effectiveOptions(spec.MountOptions)
	if err != nil {
		return probeFail(ReasonOther, err.Error(), "")
	}
	if !m.o.IsRoot() {
		return probeFail(ReasonNotRoot, "the agent is not running as root and cannot mount NFS", "")
	}
	// The probe directory is the agent's own; create it on first use.
	if err := os.MkdirAll(m.o.ProbeDir, 0o700); err != nil {
		return probeFail(ReasonOther, "cannot create the probe directory: "+err.Error(), "")
	}
	tmp, err := os.MkdirTemp(m.o.ProbeDir, "gpuworks-probe-")
	if err != nil {
		return probeFail(ReasonOther, "cannot create a temporary mount point: "+err.Error(), "")
	}
	key := "p:" + tmp
	res, err := m.runMount(ctx, key, spec.Export, tmp, opts, false)
	if err != nil {
		// The mount may have completed anyway (or finish later); clean up whatever is there.
		st := failErr(Status{}, err, "mount")
		out := probeFail(reasonOrOther(st), st.Message, "")
		if errors.Is(err, ErrUnresponsive) {
			// mount.nfs stuck past its own time limit is a server that does
			// not answer: wrong address, firewall, or a dead server.
			out = probeFail(ReasonUnreachable, fmt.Sprintf("the NFS server did not answer within %s", m.o.MountTimeout), "")
		}
		if note := m.cleanupProbe(context.WithoutCancel(ctx), key, tmp); note != "" {
			out.Message += " " + note
		}
		return out
	}
	if res.err != nil || res.timedOut {
		os.Remove(tmp)
		reason, msg := mountFailure(res, m.o.MountTimeout)
		return probeFail(reason, msg, truncate(strings.TrimSpace(string(res.out)), 400))
	}

	out := m.probeMounted(ctx, key, tmp)
	out.Output = truncate(strings.TrimSpace(string(res.out)), 400)
	// Clean up even when ctx is already cancelled.
	if note := m.cleanupProbe(context.WithoutCancel(ctx), key, tmp); note != "" {
		out.Message = strings.TrimSpace(out.Message + " " + note)
	}
	return out
}

func (m *Manager) probeMounted(ctx context.Context, key, tmp string) ProbeResult {
	res := ProbeResult{}
	if e, ok := m.mountAt(tmp); ok {
		res.Version = superOption(e.SuperOptions, "vers")
		res.Options = mergeOptions(e.Options, e.SuperOptions)
	}
	if err := m.call(ctx, key, func() error { return checkReadableDir(tmp) }); err != nil {
		st := failErr(Status{}, err, "read export root")
		r := probeFail(reasonOrOther(st), st.Message, "")
		r.Version, r.Options = res.Version, res.Options
		return r
	}
	res.Readable = true
	return m.probeFinish(ctx, key, tmp, filepath.Join(tmp, MetaDirName), false, res)
}

// cleanupProbe unmounts and removes the temporary mount point. It returns a note when something
// was left behind.
func (m *Manager) cleanupProbe(ctx context.Context, key, tmp string) string {
	if _, mounted := m.mountAt(tmp); !mounted {
		_ = os.Remove(tmp)
		return ""
	}
	out, cmdErr, err := m.runUmount(ctx, key+":umount", tmp)
	switch {
	case err != nil:
		m.o.Logger.Warn("storagemount: probe umount did not return", "path", tmp)
		return "the probe mount at " + tmp + " could not be unmounted (no response)"
	case cmdErr != nil:
		m.o.Logger.Warn("storagemount: probe umount failed", "path", tmp, "err", cmdErr)
		return "the probe mount at " + tmp + " could not be unmounted: " + truncate(strings.TrimSpace(string(out)), 200)
	}
	_ = os.Remove(tmp)
	return ""
}
