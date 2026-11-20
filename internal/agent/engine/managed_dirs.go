// managed_dirs.go keeps the writable directories inference templates declare
// (proto.MountPurposeManagedDir, ADR 0077), the deployment-lifetime sibling of
// the KV disk tier (kv_disk_sweeper.go): both are prepared by the same
// RuntimeMountManager under the runtime mount root and filled into the
// workload the same way (setManagedMountSource); they differ in lifetime and
// in that only the KV disk, a cache, is pruned. Each lives at
// <runtime mount root>/managed-dir/<group>/<name>, where the group names the
// deployment (proto.ManagedDirGroupKey). A directory outlives the instances
// that mount it - a group re-formed on this node finds what its predecessor
// left - and goes only when the server says its deployment is gone.

package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// PrepareManagedDir creates (or finds) one managed directory, owned by the
// runtime mount owner and writable by it, and answers with its host path.
func (m *RuntimeMountManager) PrepareManagedDir(group, name string) (string, error) {
	if _, _, ok := proto.ParseManagedDirGroupKey(group); !ok {
		return "", fmt.Errorf("invalid managed directory group %q", group)
	}
	if !proto.ValidManagedDirName(name) {
		return "", fmt.Errorf("invalid managed directory name %q", name)
	}
	groupDir, err := m.PrepareInstanceDir(proto.MountPurposeManagedDir, group)
	if err != nil {
		return "", err
	}
	owner, err := m.mountOwner()
	if err != nil {
		return "", err
	}
	destination := filepath.Join(groupDir, name)
	if err := ensureManagedDirectory(destination, owner, 0o755, true, m.createAsOwner); err != nil {
		return "", fmt.Errorf("prepare managed directory %s: %w", destination, err)
	}
	if err := m.preflight(destination, owner, true); err != nil {
		return "", err
	}
	return destination, nil
}

// applyManagedDirMounts fills in the host source of every managed directory
// mount of the launch. The server states the container path and which
// directory it is; the host path is the node's decision.
func (e *Engine) applyManagedDirMounts(ws *runtime.WorkloadSpec, spec proto.InstanceDesiredState) error {
	for _, mount := range spec.Mounts {
		if mount.Purpose != proto.MountPurposeManagedDir || mount.Source != "" {
			continue
		}
		if mount.ManagedDir == nil {
			return fmt.Errorf("managed directory mount %s names no directory", mount.Target)
		}
		dir, err := e.runtimeMountManager().PrepareManagedDir(mount.ManagedDir.Group, mount.ManagedDir.Name)
		if err != nil {
			return err
		}
		setManagedMountSource(ws, mount.Target, dir)
	}
	return nil
}

// setManagedMountSource writes the host directory the node prepared for a
// managed mount (a template's dir or the KV disk tier) into the workload.
func setManagedMountSource(ws *runtime.WorkloadSpec, target, dir string) {
	target = filepath.Clean(target)
	for index := range ws.Mounts {
		if filepath.Clean(ws.Mounts[index].Target) == target {
			ws.Mounts[index].Source = dir
		}
	}
}

// ManagedDirGroups lists the managed directory groups on this node, for the
// hello (proto.Hello.ManagedDirGroups). Only entries named like a group are
// listed; nothing else under the root is the platform's to report or remove.
func (e *Engine) ManagedDirGroups() []string {
	root := e.runtimeMountManager().purposeRoot(proto.MountPurposeManagedDir)
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			e.log.Warn("could not list managed directory groups", "dir", root, "err", err)
		}
		return nil
	}
	var groups []string
	for _, entry := range entries {
		if _, _, ok := proto.ParseManagedDirGroupKey(entry.Name()); ok && entry.IsDir() {
			groups = append(groups, entry.Name())
		}
	}
	sort.Strings(groups)
	return groups
}

// ReleaseManagedDirGroups removes the named groups (a replay's
// proto.DesiredState.ReleasedDirGroups). A group a desired instance still
// mounts is kept: the server named it from a deployment it no longer has,
// and an instance of it here is about to be torn down; the next connection
// names the group again.
func (e *Engine) ReleaseManagedDirGroups(groups []string) {
	for _, group := range groups {
		if _, _, ok := proto.ParseManagedDirGroupKey(group); ok {
			e.releaseManagedDirGroup(group)
		}
	}
}

// ReleaseManagedDirDeployments removes every group of the named deployments
// (proto.StateUpdate.ReleasedDirDeployments).
func (e *Engine) ReleaseManagedDirDeployments(deploymentIDs []uint) {
	if len(deploymentIDs) == 0 {
		return
	}
	released := make(map[uint]bool, len(deploymentIDs))
	for _, id := range deploymentIDs {
		released[id] = true
	}
	for _, group := range e.ManagedDirGroups() {
		if id, _, _ := proto.ParseManagedDirGroupKey(group); released[id] {
			e.releaseManagedDirGroup(group)
		}
	}
}

func (e *Engine) releaseManagedDirGroup(group string) {
	if e.mountsManagedDirGroup(group) {
		e.log.Info("keeping a released managed directory group a desired instance still mounts", "group", group)
		return
	}
	root := e.runtimeMountManager().purposeRoot(proto.MountPurposeManagedDir)
	if root == "" {
		return
	}
	dir := filepath.Join(root, group)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return
	}
	e.log.Info("removing a released deployment's managed directories", "group", group, "dir", dir)
	if err := os.RemoveAll(dir); err != nil {
		e.log.Warn("could not remove a released deployment's managed directories", "group", group, "dir", dir, "err", err)
	}
}

// mountsManagedDirGroup reports whether a desired instance mounts a directory
// of the group.
func (e *Engine) mountsManagedDirGroup(group string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, spec := range e.desired {
		for _, mount := range spec.Mounts {
			if mount.Purpose == proto.MountPurposeManagedDir && mount.ManagedDir != nil && mount.ManagedDir.Group == group {
				return true
			}
		}
	}
	return false
}

// ManagedDirRoot is the host directory every managed directory group lives
// in, created on first use; the node reports its filesystem's free space
// (proto.PathKindManagedDirs).
func (e *Engine) ManagedDirRoot() string {
	return e.runtimeMountManager().purposeRoot(proto.MountPurposeManagedDir)
}
