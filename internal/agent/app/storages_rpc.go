package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (n *NodeRuntime) rpcStorageProbe(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.StorageProbeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if n.storage == nil {
		return nil, errors.New("storages are not available on this agent")
	}
	// The server validates a definition before it probes it, so a local
	// storage always arrives with its directory; the mount manager reports a
	// missing one as path_missing.
	spec := storagemount.Spec{ID: 0, Name: "probe", Type: p.Type, Path: p.Path, Export: p.Export, MountOptions: p.MountOptions, Existing: p.Existing}
	res := n.storage.manager.Probe(ctx, spec)
	return proto.StorageProbeResult{OK: res.OK, Reason: res.Reason, Message: res.Message, Output: res.Output,
		Version: res.Version, Options: res.Options, Readable: res.Readable, Writable: res.Writable,
		TotalBytes: res.TotalBytes, AvailableBytes: res.AvailableBytes}, nil
}

func (n *NodeRuntime) rpcStorageScan(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.StorageScanParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	root, _, err := n.readyMount(p.StorageID)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 5*time.Minute {
		timeout = 50 * time.Second
	}
	var res modelscan.Result
	err = n.storageDo(p.StorageID, "scan", timeout+n.storageTimeouts().scanGrace, func() error {
		page, err := modelscan.Scan(ctx, root, modelscan.Options{MaxDepth: p.MaxDepth, Limit: p.Limit, Cursor: p.Cursor, Deadline: time.Now().Add(timeout)})
		res = page
		return err
	})
	if err != nil {
		return nil, err
	}
	out := proto.StorageScanResult{NextCursor: res.NextCursor, Complete: res.Complete,
		Candidates: make([]proto.StorageScanCandidate, 0, len(res.Candidates)), Skipped: make([]proto.StorageScanSkipped, 0, len(res.Skipped))}
	for _, c := range res.Candidates {
		var issues []string
		for _, issue := range c.Issues {
			issues = append(issues, issue.Code)
		}
		out.Candidates = append(out.Candidates, proto.StorageScanCandidate{RelPath: c.RelPath, SizeBytes: c.SizeBytes, Files: c.Files,
			Source: c.Source, SourceID: c.SourceID, Revision: c.Revision, Subfolder: c.Subfolder, Basis: c.Basis, Format: c.Format, Tasks: c.Tasks,
			Incomplete: c.Incomplete, IncompleteReason: c.IncompleteReason, MetadataIssues: issues})
	}
	for _, s := range res.Skipped {
		out.Skipped = append(out.Skipped, proto.StorageScanSkipped{RelPath: s.RelPath, Reason: s.Reason})
	}
	return out, nil
}

func (n *NodeRuntime) rpcStoragePeers(raw json.RawMessage) (any, error) {
	var p proto.StoragePeersParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	root, status, err := n.readyMount(p.StorageID)
	if err != nil {
		return nil, err
	}
	if !status.Writable {
		return nil, errors.New("this node cannot write to the storage")
	}
	if p.Remove {
		return proto.StoragePeersResult{Peers: []proto.StoragePeer{}}, n.storageDo(p.StorageID, "peers", n.storageTimeouts().peers, func() error {
			return storagemount.RemoveHeartbeat(root, p.ControlPlaneID)
		})
	}
	if p.FreshSeconds <= 0 {
		return nil, errors.New("storage peers request has no positive fresh_seconds")
	}
	fresh := time.Duration(p.FreshSeconds) * time.Second
	var peers []storagemount.Peer
	err = n.storageDo(p.StorageID, "peers", n.storageTimeouts().peers, func() error {
		if _, err := storagemount.WriteHeartbeat(root, storagemount.Heartbeat{ID: p.ControlPlaneID, Name: p.ControlPlaneName, Version: p.Version}); err != nil {
			return err
		}
		var err error
		peers, err = storagemount.ReadPeers(root, p.ControlPlaneID, fresh)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := proto.StoragePeersResult{Peers: make([]proto.StoragePeer, 0, len(peers))}
	for _, peer := range peers {
		out.Peers = append(out.Peers, proto.StoragePeer{ID: peer.ID, Name: peer.Name, Version: peer.Version,
			LastSeenSeconds: int64(peer.LastSeen / time.Second), Fresh: peer.Fresh})
	}
	return out, nil
}

// rpcStorageRemove deletes a model directory from a shared storage: it is
// renamed into .gpuworks/trash first, so no node ever reads a half-deleted
// model, and the bytes are removed in the background.
func (n *NodeRuntime) rpcStorageRemove(raw json.RawMessage) (any, error) {
	var p proto.StorageRemoveParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	root, status, err := n.readyMount(p.StorageID)
	if err != nil {
		return nil, err
	}
	if !status.Writable {
		return nil, errors.New("this node cannot write to the storage")
	}
	rel := filepath.Clean(strings.TrimSpace(p.RelPath))
	target, err := proto.JoinWithin(root, rel)
	if err != nil || strings.HasPrefix(rel, proto.StorageMetaDir) {
		return nil, fmt.Errorf("refusing to remove %q from the storage", p.RelPath)
	}
	trash := storagemount.TrashPath(root, rel, time.Now())
	err = n.storageDo(p.StorageID, "remove/"+rel, n.storageTimeouts().remove, func() error {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(trash), 0o755); err != nil {
			return err
		}
		return os.Rename(target, trash)
	})
	if err != nil {
		return nil, err
	}
	go func() {
		if err := os.RemoveAll(trash); err != nil {
			n.log.Warn("removing deleted model from storage failed", "path", trash, "err", err)
		}
	}()
	return map[string]any{"removed": rel}, nil
}
