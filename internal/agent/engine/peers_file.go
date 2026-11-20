package engine

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// peersMountPurpose is the runtime mount purpose of the per-instance
// directory that holds an instance's peers file.
const peersMountPurpose = "peers"

func (e *Engine) peersDir(uuid string) string {
	return filepath.Join(e.runtimeMountManager().purposeRoot(peersMountPurpose), sanitizeID(uuid))
}

// applyPeersFile writes the launch's peers file (proto.PeersFile) to the
// instance's own host directory and mounts it read-only at
// proto.PeersFileMountPath; the server already set GPUWORKS_PEERS_FILE to that
// path. The file is rewritten on every container start, so a restarted
// container reads what the desired state says now, and it goes with the
// instance (reclaimInstance). A launch without peers gets nothing.
func (e *Engine) applyPeersFile(ws *runtime.WorkloadSpec, spec proto.InstanceDesiredState) error {
	if spec.Peers == nil {
		return nil
	}
	content, err := json.MarshalIndent(spec.Peers, "", "  ")
	if err != nil {
		return fmt.Errorf("encode peers file: %w", err)
	}
	content = append(content, '\n')
	manager := e.runtimeMountManager()
	dir, err := manager.PrepareInstanceDir(peersMountPurpose, sanitizeID(spec.UUID))
	if err != nil {
		return fmt.Errorf("create peers directory: %w", err)
	}
	owner, err := manager.mountOwner()
	if err != nil {
		return fmt.Errorf("resolve peers file owner: %w", err)
	}
	hostPath, err := manager.writeReadOnlyFileAtomic(dir, "peers.json", content, owner)
	if err != nil {
		return fmt.Errorf("write peers file: %w", err)
	}
	ws.Mounts = append(ws.Mounts, runtime.Mount{Source: hostPath, Target: proto.PeersFileMountPath, ReadOnly: true})
	return nil
}
