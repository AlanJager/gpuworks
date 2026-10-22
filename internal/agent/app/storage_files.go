package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagefiles"
	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// storageFilesSlots is how many calls of one storage-files operation may run
// side by side on one storage: enough for a few admins browsing at once,
// bounded so a hung mount holds few goroutines.
const storageFilesSlots = 4

// storageFiles is the storage-files service (ADR 0053) bound to this node's
// storages. It is cheap to build; everything it knows is read per call.
func (n *NodeRuntime) storageFiles() *storagefiles.Service {
	o := storagefiles.Options{Root: n.storageFilesRoot, Log: n.log}
	if n.storage != nil {
		manager := n.storage.manager
		o.Do = func(id uint, op string, timeout time.Duration, fn func() error) error {
			return manager.DoAny(id, op, storageFilesSlots, timeout, fn)
		}
		o.Available = func(path string) (int64, error) {
			u, err := manager.Statfs(path)
			return u.AvailableBytes, err
		}
	}
	return storagefiles.New(o)
}

// storageFilesRoot is a storage's root on this node and whether it is usable:
// a local storage's directory or a shared storage's mount point (for
// nfs_cache the NFS mount, never the node cache), as the last check found it.
func (n *NodeRuntime) storageFilesRoot(id uint) (storagefiles.Root, error) {
	if n.storage == nil {
		return storagefiles.Root{}, errors.New("storages are not available on this agent")
	}
	n.storage.mu.RLock()
	task, ok := n.storage.tasks[id]
	status, checked := n.storage.statuses[id]
	n.storage.mu.RUnlock()
	if !ok {
		return storagefiles.Root{}, fmt.Errorf("storage %d is not configured on this node", id)
	}
	ready := checked && status.State == storagemount.StateReady
	return storagefiles.Root{Dir: n.storageSpec(task).Path, Ready: ready, Writable: ready && status.Writable}, nil
}

func decodeStorageFiles[T any](raw json.RawMessage) (T, error) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, proto.NewStorageFilesError(proto.StorageFilesErrFailed, "decode request: %v", err)
	}
	return p, nil
}

func (n *NodeRuntime) rpcStorageFilesList(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeStorageFiles[proto.StorageFilesListParams](raw)
	if err != nil {
		return nil, err
	}
	return n.storageFiles().List(ctx, p)
}

func (n *NodeRuntime) rpcStorageFilesMkdir(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeStorageFiles[proto.StorageFilesMkdirParams](raw)
	if err != nil {
		return nil, err
	}
	return n.storageFiles().Mkdir(ctx, p)
}

func (n *NodeRuntime) rpcStorageFilesRename(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeStorageFiles[proto.StorageFilesRenameParams](raw)
	if err != nil {
		return nil, err
	}
	return n.storageFiles().Rename(ctx, p)
}

func (n *NodeRuntime) rpcStorageFilesDelete(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeStorageFiles[proto.StorageFilesDeleteParams](raw)
	if err != nil {
		return nil, err
	}
	return n.storageFiles().Delete(ctx, p)
}

// ReceiveStorageUpload serves a StreamStorageUpload stream.
func (n *NodeRuntime) ReceiveStorageUpload(ctx context.Context, hdr proto.StreamHeader, rw io.ReadWriter) error {
	return n.storageFiles().Upload(ctx, hdr, rw)
}

// SendStorageDownload serves a StreamStorageDownload stream.
func (n *NodeRuntime) SendStorageDownload(ctx context.Context, hdr proto.StreamHeader, w io.Writer) error {
	return n.storageFiles().Download(ctx, hdr, w)
}

// sweepIncoming removes the upload staging an earlier agent left behind on a
// storage this node can write to.
func (n *NodeRuntime) sweepIncoming(id uint, root string) {
	err := n.storageDo(id, "incoming", n.storageTimeouts().sweep, func() error {
		_, err := storagefiles.SweepIncoming(root, storagefiles.IncomingMaxAge, time.Now())
		return err
	})
	if err != nil {
		n.log.Warn("storage upload staging sweep failed", "storage", id, "err", err)
	}
}

// sweepIncomingOnce sweeps each writable storage once per agent process, the
// first time a check finds it ready: the start-up half of the staging sweep
// (the heartbeat node repeats it with every trash sweep).
func (n *NodeRuntime) sweepIncomingOnce() {
	s := n.storage
	type due struct {
		id   uint
		root string
	}
	var work []due
	s.mu.Lock()
	for id, task := range s.tasks {
		st, ok := s.statuses[id]
		if s.incomingSwept[id] || !ok || st.State != storagemount.StateReady || !st.Writable {
			continue
		}
		s.incomingSwept[id] = true
		work = append(work, due{id: id, root: n.storageSpec(task).Path})
	}
	s.mu.Unlock()
	for _, item := range work {
		go n.sweepIncoming(item.id, item.root)
	}
}
