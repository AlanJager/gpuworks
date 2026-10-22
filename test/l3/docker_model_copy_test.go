//go:build docker

package l3

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestDockerModelCopyReadinessAndMount proves the copy barrier with a real
// container. The fake engine proves mount/lifecycle behavior, not GPU inference.
func TestDockerModelCopyReadinessAndMount(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	if err := ensureFakeImage(t, filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))); err != nil {
		t.Fatal(err)
	}
	registerFakeEngine(t, false)
	env := testenv.New(t)
	port := reservePort(t)
	env.SetControllerEnginePorts(port, port)
	env.AddNode("copy-docker", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(dockerdrv.New()))
	env.WaitNodeOnline(t, "copy-docker")
	nodes, _ := env.Store().ListNodes()
	node := nodes[0]
	api := env.API(t)
	post := func(path string, body any) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := api.PostJSON(path, raw)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("POST %s: %d %s", path, response.StatusCode, data)
		}
		return data
	}
	sourceRoot, targetRoot := t.TempDir(), t.TempDir()
	storage := func(name, path string) uint {
		var row struct{ ID uint }
		if err := json.Unmarshal(post("/api/v1/storages", map[string]any{"name": name, "type": "local", "path": path, "enabled": true, "all_nodes": true}), &row); err != nil {
			t.Fatal(err)
		}
		return row.ID
	}
	sourceID, targetID := storage("copy-source", sourceRoot), storage("copy-target", targetRoot)
	source := filepath.Join(sourceRoot, "tiny")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`)
	for name, content := range map[string][]byte{"config.json": config, "model.safetensors": bytes.Repeat([]byte("copy"), 3<<20)} {
		if err := os.WriteFile(filepath.Join(source, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	var model struct{ ID uint }
	if err := json.Unmarshal(post("/api/v1/models/local", map[string]any{"node_uuid": node.UUID, "storage_id": sourceID, "path": source, "name": "copy-model"}), &model); err != nil {
		t.Fatal(err)
	}
	rate := 1024
	if os.Getenv("GPUWORKS_COPY_BROWSER") == "1" {
		rate = 256
	}
	post(fmt.Sprintf("/api/v1/models/%d/copy", model.ID), map[string]any{"source": map[string]any{"storage_id": sourceID, "node_uuid": node.UUID}, "target": map[string]any{"storage_id": targetID, "node_uuid": node.UUID}, "rate_limit_kib_s": rate})
	var dep dto.Deployment
	if err := json.Unmarshal(post("/api/v1/deployments", map[string]any{"model_id": model.ID, "node_uuid": node.UUID, "served_name": "copy-model", "engine": "fake-engine", "image": "gpuworks/fake-engine:test", "node_requirements": map[string][]string{"gpuworks.ai/storage": {fmt.Sprint(targetID)}}}), &dep); err != nil {
		t.Fatal(err)
	}
	read := func() dto.Deployment {
		t.Helper()
		response, err := api.Get(fmt.Sprintf("/api/v1/deployments/%d", dep.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var value dto.Deployment
		if response.StatusCode != http.StatusOK {
			t.Fatal(response.StatusCode)
		}
		if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	env.Eventually(t, 20*time.Second, func() bool {
		current := read()
		for _, row := range current.ModelProgress {
			if row.StorageID == targetID && row.Downloaded > 0 && row.Downloaded < row.SizeBytes && row.Preparation != nil && row.Preparation.Phase == "copying" {
				instances, _ := env.Store().InstancesForDeployment(dep.ID)
				if len(instances) != 0 {
					t.Fatal("container intent created before copy ready")
				}
				t.Logf("partial copy %d/%d, no instance", row.Downloaded, row.SizeBytes)
				return true
			}
		}
		return false
	})
	if os.Getenv("GPUWORKS_COPY_BROWSER") == "1" {
		output := os.Getenv("GPUWORKS_COPY_BROWSER_OUTPUT")
		if output == "" {
			t.Fatal("GPUWORKS_COPY_BROWSER_OUTPUT is required")
		}
		script := filepath.Clean(filepath.Join(filepath.Dir(file), "../e2e/model_copy_preparation.py"))
		command := exec.Command("python3", script, "--base", env.URL(), "--deployment", fmt.Sprint(dep.ID), "--storage", fmt.Sprint(targetID), "--output", output)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("browser: %v\n%s", err, output)
		}
	}
	var containerID, target string
	env.Eventually(t, 60*time.Second, func() bool {
		instances, _ := env.Store().InstancesForDeployment(dep.ID)
		if len(instances) != 1 || instances[0].State != proto.InstRunning {
			return false
		}
		containerID = instances[0].ContainerID
		for _, row := range read().ModelProgress {
			if row.StorageID == targetID {
				target = row.Path
			}
		}
		return target != ""
	})
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })
	output, err := exec.Command("docker", "inspect", containerID).Output()
	if err != nil {
		t.Fatal(err)
	}
	var inspected []struct {
		Mounts []struct {
			Source, Destination string
			RW                  bool
		}
	}
	if err := json.Unmarshal(output, &inspected); err != nil {
		t.Fatal(err)
	}
	mounted := false
	for _, mount := range inspected[0].Mounts {
		if mount.Destination == "/model" {
			mounted = mount.Source == target && !mount.RW
		}
	}
	if !mounted {
		t.Fatalf("wrong model mount: target=%s mounts=%+v", target, inspected)
	}
	got, err := os.ReadFile(filepath.Join(target, "config.json"))
	if err != nil || !bytes.Equal(got, config) {
		t.Fatalf("installed config differs: %v", err)
	}
	t.Logf("running container mounts verified copy read-only at /model")
}
