package deploy

import (
	"encoding/json"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// peersFileEnv is deploy.PeersFileEnv.
const peersFileEnv = "GPUWORKS_PEERS_FILE"

// assertPeersFileMounted checks what a rank's container was given to find its
// peers: the variable naming the file, the file mounted read-only there, and
// in it every rank of the group with the address it advertises. Host names
// and management addresses depend on the fake agent's host, so only the
// facts the test placed are compared.
func assertPeersFileMounted(t *testing.T, spec runtime.WorkloadSpec, rank int, masterAddr string, masterPort int, want []proto.PeerRank) {
	t.Helper()
	if spec.Env[peersFileEnv] != proto.PeersFileMountPath {
		t.Fatalf("rank %d env %s = %q", rank, peersFileEnv, spec.Env[peersFileEnv])
	}
	source := ""
	for _, mount := range spec.Mounts {
		if mount.Target == proto.PeersFileMountPath {
			if !mount.ReadOnly {
				t.Fatalf("rank %d peers file is mounted writable: %+v", rank, mount)
			}
			source = mount.Source
		}
	}
	if source == "" {
		t.Fatalf("rank %d has no peers file mount: %+v", rank, spec.Mounts)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("rank %d peers file: %v", rank, err)
	}
	var file proto.PeersFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("rank %d peers file %s: %v", rank, raw, err)
	}
	if file.Version != proto.PeersFileVersion || file.NodeRank != rank || file.NNodes != len(want) || len(file.Ranks) != len(want) ||
		file.MasterAddr != masterAddr || file.MasterPort != masterPort {
		t.Fatalf("rank %d peers file = %s", rank, raw)
	}
	for i, peer := range file.Ranks {
		if peer.NodeRank != want[i].NodeRank || peer.NodeName != want[i].NodeName || peer.NodeUUID != want[i].NodeUUID || peer.NodeAddr != want[i].NodeAddr {
			t.Fatalf("rank %d lists peer %d as %+v, want %+v", rank, i, peer, want[i])
		}
	}
}
