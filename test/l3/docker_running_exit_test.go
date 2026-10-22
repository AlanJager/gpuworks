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

	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// `docker stop` on a running instance's container, from outside the platform:
// the agent reports the instance failed with the container's exit code, the
// controller rebuilds it as a new attempt, and deleting the deployment leaves
// no container behind.
func TestDockerStoppedRunningContainerIsReportedAndRebuilt(t *testing.T) {
	deploy.RegisterEngine(fakeEngineProfile{})
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	env := testenv.New(t)
	env.SetControllerRetryPolicy(100*time.Millisecond, 3)
	node := env.AddNode("docker-exit-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(dockerdrv.New()))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fake-exit", Revision: "main", Name: "fake-exit", Arch: "qwen3", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: modelDir}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "fake-exit", "engine": "fake-engine", "image": "gpuworks/fake-engine:test"})
	created, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil || created.StatusCode != http.StatusCreated {
		if created != nil {
			data, _ := io.ReadAll(created.Body)
			created.Body.Close()
			t.Fatalf("create deployment status=%d body=%s err=%v", created.StatusCode, data, err)
		}
		t.Fatalf("create deployment err=%v", err)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(created.Body).Decode(&dep)
	created.Body.Close()

	running := func(skip string) *store.Instance {
		instances, _ := env.Store().ListInstances()
		for i := range instances {
			if instances[i].UUID != skip && instances[i].State == proto.InstRunning && instances[i].ContainerID != "" {
				return &instances[i]
			}
		}
		return nil
	}
	env.Eventually(t, 45*time.Second, func() bool { return running("") != nil })
	first := running("")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", first.ContainerID).Run() })

	if out, err := exec.Command("docker", "stop", first.ContainerID).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v: %s", err, out)
	}
	// One liveness interval (15 s) plus the report's trip to the server.
	env.Eventually(t, 45*time.Second, func() bool {
		inst, err := env.Store().InstanceByUUID(first.UUID)
		return err == nil && inst.State == proto.InstFailed
	})
	failed, _ := env.Store().InstanceByUUID(first.UUID)
	t.Logf("reported after docker stop: %q", failed.Message)
	if !strings.HasPrefix(failed.Message, "container exited while running (code ") {
		t.Fatalf("failure message = %q", failed.Message)
	}
	// The agent reclaimed the stopped container before reporting.
	if _, err := exec.Command("docker", "inspect", first.ContainerID).CombinedOutput(); err == nil {
		t.Fatalf("stopped container %s is still present", first.ContainerID)
	}

	env.Eventually(t, 45*time.Second, func() bool { return running(first.UUID) != nil })
	second := running(first.UUID)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", second.ContainerID).Run() })
	if second.ContainerID == first.ContainerID {
		t.Fatalf("rebuild reused the stopped container %s", second.ContainerID)
	}
	if out, err := exec.Command("docker", "inspect", second.ContainerID, "--format", "{{.State.Running}}").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("rebuilt container is not running: %v %s", err, out)
	}
	// The failure was reported once: the failed row did not move on.
	if again, _ := env.Store().InstanceByUUID(first.UUID); again.State != proto.InstFailed || again.Message != failed.Message {
		t.Fatalf("failed attempt changed to %s %q", again.State, again.Message)
	}

	deleted, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(dep.ID), 10))
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
	env.Eventually(t, 30*time.Second, func() bool {
		_, inspectErr := exec.Command("docker", "inspect", second.ContainerID).CombinedOutput()
		return inspectErr != nil
	})
}
