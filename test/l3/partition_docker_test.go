//go:build docker

package l3

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestPartitionBackendsReachRealDockerAndCleanInstanceState(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		t.Skip(err)
	}
	registerFakeEngine(t, false)

	tests := []struct {
		name, vendor, framework, version, model, archID, backend string
		files                                                    []string
		wantEnv                                                  map[string]string
		wantMounts                                               []string
		domesticState                                            bool
	}{
		{name: "nvidia", vendor: "nvidia", framework: "cuda", version: "12.4", model: "NVIDIA A100", archID: "sm_80", backend: accelerator.BackendNVIDIALogical,
			wantEnv: map[string]string{"LD_PRELOAD": "/gpuworks/libvgpu.so", "CUDA_DEVICE_MEMORY_LIMIT_0": "8589934592"}, wantMounts: []string{"/gpuworks/libvgpu.so", "/gpuworks/cache"}},
		{name: "ascend", vendor: "ascend", framework: "cann", version: "9.0.0", model: "Ascend 910B4", archID: "910B4", backend: accelerator.BackendAscendLogical,
			files: []string{"libvruntime.so"}, wantEnv: map[string]string{"ENPU_DSMI_HOOK": "1"}, wantMounts: []string{"/opt/gpuworks/partition/libvruntime.so", "/etc/ld.so.preload", "/etc/enpu/vcann-rt/npu_info.config"}, domesticState: true},
		{name: "ppu", vendor: "ppu", framework: "hggc", version: "13.0", model: "PPU-ZW810E", archID: "Beethoven", backend: accelerator.BackendPPULogical,
			files: []string{"hggc_quota.so", "hgml_dlsym_hook.so"}, wantEnv: map[string]string{"NVIDIA_VISIBLE_DEVICES": "void", "HGGC_DEVICE_MEMORY_LIMIT_0": "8192", "LD_PRELOAD": "/opt/gpuworks/partition/hgml_dlsym_hook.so:/opt/gpuworks/partition/hggc_quota.so"}, wantMounts: []string{"/opt/gpuworks/partition/hggc_quota.so", "/opt/gpuworks/partition/hgml_dlsym_hook.so"}, domesticState: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.files) > 0 {
				assetRoot := t.TempDir()
				assetDir := filepath.Join(assetRoot, tt.name, "linux-"+runtime.GOARCH, tt.framework+"-"+tt.version)
				if err := os.MkdirAll(assetDir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, name := range tt.files {
					if err := os.WriteFile(filepath.Join(assetDir, name), []byte("l3-fake-runtime-library"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("GPUWORKS_PARTITION_ASSETS", assetRoot)
			}
			env := testenv.New(t)
			apiPort := reservePort(t)
			env.SetControllerEnginePorts(apiPort, apiPort)
			driver := newProbeStandInDriver()
			node := env.AddNode("docker-partition-"+tt.name,
				testenv.WithAccelerator(tt.vendor, tt.framework, tt.version),
				testenv.WithGPUs(1, testenv.GPUSpec{Model: tt.model, Vendor: tt.vendor, ArchID: tt.archID, VRAMMB: 32768}),
				testenv.WithDriver(driver), testenv.WithoutAcceleratorRuntimeInjection(),
			)
			env.WaitNodeOnline(t, node.Name)
			env.Eventually(t, 15*time.Second, func() bool {
				row, err := env.Store().NodeWithGPUs(node.UUID)
				return err == nil && strings.Contains(row.PartitionAssets, tt.backend)
			})
			// The agent reports the partition probe it ran itself; the probe
			// container is the stand-in driver's, so the report is a pass. No
			// status is written by hand: any later report of the agent would
			// replace it.
			if tt.domesticState {
				env.Eventually(t, 60*time.Second, func() bool {
					row, err := env.Store().NodeWithGPUs(node.UUID)
					return err == nil && strings.Contains(row.PartitionProbes, `"status":"`+proto.PartitionProbePassed+`"`) && strings.Contains(row.PartitionProbes, tt.backend)
				})
				launches := driver.probeLaunches()
				if len(launches) == 0 {
					t.Fatal("the agent reported a passing probe without starting one")
				}
				if launches[0].Env["GPUWORKS_PARTITION_PROBE_BACKEND"] != tt.backend {
					t.Fatalf("probe workload env = %+v, want backend %s", launches[0].Env, tt.backend)
				}
			}
			if _, err := env.Store().UpsertImage(&store.Image{ImageRef: "gpuworks/fake-engine:test", Source: "community", Engine: "fake-engine", Framework: tt.framework, FrameworkVersion: tt.version, Arch: "multi", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			row, err := env.Store().NodeWithGPUs(node.UUID)
			if err != nil {
				t.Fatal(err)
			}
			modelDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "l3/" + tt.name, Revision: "main", Name: "l3-" + tt.name, Arch: "qwen3", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: modelDir}); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"name": "l3-" + tt.name, "served_name": "l3-" + tt.name, "model_id": model.ID, "node_uuid": node.UUID, "engine": "fake-engine", "image": "gpuworks/fake-engine:test", "partition_backend": tt.backend, "vram_limit_gib": 8})
			resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusCreated {
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("create status=%d body=%s", resp.StatusCode, raw)
			}
			var dep struct {
				ID uint `json:"id"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			env.Eventually(t, 45*time.Second, func() bool {
				rows, _ := env.Store().ListInstances()
				return len(rows) == 1 && rows[0].State == proto.InstRunning && rows[0].ContainerID != ""
			})
			instances, _ := env.Store().ListInstances()
			instance, containerID := instances[0], instances[0].ContainerID
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })
			var inspect struct {
				Config struct{ Env []string }
				Mounts []struct{ Destination string }
			}
			out, err := exec.Command("docker", "inspect", containerID).Output()
			if err != nil {
				t.Fatalf("docker inspect: %v", err)
			}
			var inspected []struct {
				Config struct{ Env []string }
				Mounts []struct{ Destination string }
			}
			if err := json.Unmarshal(out, &inspected); err != nil || len(inspected) != 1 {
				t.Fatalf("inspect decode err=%v body=%s", err, out)
			}
			inspect = inspected[0]
			envMap := map[string]string{}
			for _, pair := range inspect.Config.Env {
				if i := strings.IndexByte(pair, '='); i > 0 {
					envMap[pair[:i]] = pair[i+1:]
				}
			}
			for key, want := range tt.wantEnv {
				if envMap[key] != want {
					t.Fatalf("container env %s=%q want %q; env=%+v", key, envMap[key], want, envMap)
				}
			}
			mounts := map[string]bool{}
			for _, mount := range inspect.Mounts {
				mounts[mount.Destination] = true
			}
			for _, target := range tt.wantMounts {
				if !mounts[target] {
					t.Fatalf("container mount %s missing: %+v", target, inspect.Mounts)
				}
			}
			statePath := filepath.Join(node.StateDir(), "dgpu-runtime", "instances", instance.UUID)
			if tt.domesticState {
				if _, err := os.Stat(statePath); err != nil {
					t.Fatalf("instance state missing before teardown: %v", err)
				}
			}
			deleted, err := env.API(t).Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(dep.ID), 10))
			if err != nil {
				t.Fatal(err)
			}
			deleted.Body.Close()
			env.Eventually(t, 30*time.Second, func() bool {
				rows, _ := env.Store().ListInstances()
				_, stateErr := os.Stat(statePath)
				_, dockerErr := exec.Command("docker", "inspect", containerID).CombinedOutput()
				return len(rows) == 0 && (!tt.domesticState || os.IsNotExist(stateErr)) && dockerErr != nil
			})
		})
	}
}
