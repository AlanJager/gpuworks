package engine

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The agent acts on one mount purpose only (proto.MountPurposeKVDisk); every
// other purpose is a label, and the mount is the bind mount the server
// stated. Renaming a label the server sends (parameter_network became
// compute_network) therefore needs nothing from an agent of either version:
// both values, decoded from the wire, reach the container unchanged.
func TestEngineTreatsTheComputeNetworkPurposeAsALabel(t *testing.T) {
	for _, purpose := range []string{proto.MountPurposeComputeNetwork, "parameter_network"} {
		t.Run(purpose, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
			e := NewWithProber(nil, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{})

			wire := `{"uuid":"i-1","deployment_id":42,"launch":{"mounts":[{"source":"/etc/hccn.conf","target":"/etc/hccn.conf","read_only":true,"purpose":"` + purpose + `"}]}}`
			var spec proto.InstanceDesiredState
			if err := json.Unmarshal([]byte(wire), &spec); err != nil {
				t.Fatal(err)
			}
			if len(spec.Mounts) != 1 || spec.Mounts[0].Purpose != purpose {
				t.Fatalf("decoded mounts = %#v", spec.Mounts)
			}
			ws := toWorkloadSpec(spec, "container")
			if err := e.applyKVDiskMount(&ws, spec); err != nil {
				t.Fatal(err)
			}
			mount, ok := mountFor(ws, "/etc/hccn.conf")
			if !ok || mount.Source != "/etc/hccn.conf" || !mount.ReadOnly {
				t.Fatalf("mount = %#v %v, want the read-only bind mount the server stated", mount, ok)
			}
			if entries, err := os.ReadDir(filepath.Join(state, "runtime-mounts")); err == nil && len(entries) > 0 {
				t.Fatalf("a managed directory was prepared for a label purpose: %v", entries)
			}
		})
	}
}
