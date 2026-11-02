package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestHygonUsesNativeVdevConfigFilename(t *testing.T) {
	for _, model := range []string{"Z100", "K100AI", "K100_AI", "K100 AI"} {
		for _, vdevID := range []int{0, 1, 4} {
			t.Run(fmt.Sprintf("%s/vdev%d", model, vdevID), func(t *testing.T) {
				e, h, helper := hygonTestEngine(t)
				helper.nextID = vdevID
				p := e.prober.(*hygonTestProber)
				p.GPUs[0].ArchID = model
				if model != "Z100" {
					p.GPUs[0].MemoryTotalBytes = 65520 << 20
					p.computeUnits = 128
				}
				ws := &agentruntime.WorkloadSpec{Image: "hygon/inference:test"}
				if ok, reason := h.apply(e, context.Background(), ws, proto.InstanceDesiredState{
					UUID: "filename", GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20,
				}); !ok {
					t.Fatal(reason)
				}
				nativeName := fmt.Sprintf("vdev%d.conf", vdevID)
				var snapshot string
				var dockerMounts []agentruntime.Mount
				for _, mount := range ws.Mounts {
					if mount.Target == filepath.Join("/etc/vdev", nativeName) {
						if !mount.ReadOnly {
							t.Fatal("host vdev config mount is writable")
						}
						snapshot = mount.Source
					}
					if strings.HasPrefix(mount.Target, "/etc/vdev/docker/") {
						dockerMounts = append(dockerMounts, mount)
					}
				}
				if snapshot == "" || filepath.Base(snapshot) != nativeName {
					t.Fatalf("native config snapshot = %q, want %s", snapshot, nativeName)
				}
				want := agentruntime.Mount{Source: snapshot, Target: filepath.Join("/etc/vdev/docker", nativeName), ReadOnly: true}
				if len(dockerMounts) != 1 || dockerMounts[0] != want {
					t.Fatalf("Docker configs = %+v, want exactly %+v", dockerMounts, want)
				}
				if err := h.cleanupInstance(e, "filename"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
