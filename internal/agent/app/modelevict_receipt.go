package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const cacheEvictionDirectory = ".gpuworks-evictions"

// Cache eviction uses a separate RPC so an older agent cannot silently ignore
// the operation identity and delete a later copy on a delayed retry.
func (n *NodeRuntime) rpcCacheEvict(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Path       string `json:"path"`
		EvictionID string `json:"eviction_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Path) == "" || strings.TrimSpace(p.EvictionID) == "" || len(p.EvictionID) > 256 {
		return nil, fmt.Errorf("cache eviction requires path and eviction_id (at most 256 bytes)")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n.stateDir == "" {
		return nil, fmt.Errorf("cache eviction requires durable agent state directory")
	}
	target := filepath.Clean(n.resolveModelPath(p.Path))
	if pathWithin(target, filepath.Clean(n.stateDir)) {
		return nil, fmt.Errorf("cache eviction cannot remove agent state directory")
	}
	var storageRoot string
	for _, candidate := range n.storageRoots() {
		if pathWithin(candidate, target) && target != candidate && len(candidate) > len(storageRoot) {
			storageRoot = candidate
		}
	}
	if storageRoot == "" {
		return nil, fmt.Errorf("cache eviction path is outside models root")
	}
	rel, err := filepath.Rel(storageRoot, target)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == cacheEvictionDirectory {
			return nil, fmt.Errorf("cache eviction cannot remove its receipt directory")
		}
	}
	root, err := os.OpenRoot(storageRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	state, err := os.OpenRoot(n.stateDir)
	if err != nil {
		return nil, err
	}
	defer state.Close()
	receipt := cacheEvictionReceipt(target, p.EvictionID)
	remove := func() error {
		return removeCacheWithReceipt(ctx, root, state, rel, receipt, root.RemoveAll)
	}
	err = n.removeModelPathWith(ctx, target, func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		detached, err := cacheEvictionDetached(root, state, rel, receipt)
		if err != nil {
			return false, err
		}
		if detached {
			return true, remove()
		}
		if err := cacheEvictionNoSymlinks(root, rel); err != nil {
			return false, err
		}
		n.dropWaitingUnder(target)
		return false, nil
	}, remove)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": target, "removed": true, "eviction_id": p.EvictionID}, nil
}

func cacheEvictionReceipt(target, token string) string {
	return filepath.Join(cacheEvictionDirectory, fmt.Sprintf("%x", sha256.Sum256([]byte(target+"\x00"+token))))
}

// The durable marker lives in agent state, outside directories that may later
// be evicted or rebound as a storage root. The detached bytes stay beside the
// target for an atomic, same-filesystem rename. Neither path uses raw tokens.
func cacheEvictionDetached(root, state *os.Root, target, receipt string) (bool, error) {
	if err := cacheEvictionNoSymlinks(state, receipt); err != nil {
		return false, err
	}
	info, err := state.Lstat(receipt)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return false, fmt.Errorf("invalid cache eviction receipt")
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	data := filepath.Join(filepath.Dir(target), receipt, "data")
	if err := cacheEvictionNoSymlinks(root, data); err != nil {
		return false, err
	}
	_, err = root.Lstat(data)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// os.Root prevents traversal outside the authorized storage. Reject symlinks
// inside it too, so a receipt cannot redirect cleanup into a different model.
func cacheEvictionNoSymlinks(root *os.Root, rel string) error {
	current := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cache eviction refuses symlink %s", current)
		}
	}
	return nil
}

// Rename isolates this operation's bytes before deletion. A crash after rename
// is recovered from data; after removal, the durable state marker remains.
// Nothing is removed unless the rename and receipt have both been synced.
func removeCacheWithReceipt(ctx context.Context, root, state *os.Root, target, receipt string, remove func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	detached, err := cacheEvictionDetached(root, state, target, receipt)
	if err != nil {
		return err
	}
	journal := filepath.Join(filepath.Dir(target), receipt)
	data := filepath.Join(journal, "data")
	if err := cacheEvictionNoSymlinks(root, journal); err != nil {
		return err
	}
	if err := makeCacheEvictionDir(state, cacheEvictionDirectory); err != nil {
		return err
	}
	if err := syncCacheEvictionDir(state, "."); err != nil {
		return err
	}
	if !detached {
		if err := cacheEvictionNoSymlinks(root, target); err != nil {
			return err
		}
		if _, err := root.Lstat(target); err == nil {
			for _, dir := range []string{filepath.Dir(journal), journal} {
				if err := makeCacheEvictionDir(root, dir); err != nil {
					return err
				}
			}
			if err := syncCacheEvictionDir(root, filepath.Dir(journal)); err != nil {
				return err
			}
			if err := root.Rename(target, data); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Sync both sides even on a retry after rename, before making its durable
	// marker authoritative. A missing directory means there are no old bytes.
	for _, dir := range []string{filepath.Dir(target), journal} {
		if err := syncCacheEvictionDir(root, dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	f, err := state.OpenFile(receipt, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		f, err = state.Open(receipt)
	}
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	if err := syncCacheEvictionDir(state, cacheEvictionDirectory); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := remove(data); err != nil {
		return err
	}
	if err := syncCacheEvictionDir(root, journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func makeCacheEvictionDir(root *os.Root, dir string) error {
	if err := cacheEvictionNoSymlinks(root, dir); err != nil {
		return err
	}
	if err := root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := root.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("unsafe cache eviction journal %s", dir)
	}
	return nil
}

func syncCacheEvictionDir(root *os.Root, dir string) error {
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
