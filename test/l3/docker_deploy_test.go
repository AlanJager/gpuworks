//go:build docker

package l3

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

type fakeEngineProfile struct {
	engineArgs         []string
	explicitEntrypoint bool
}

func (fakeEngineProfile) Name() string { return "fake-engine" }
func (fakeEngineProfile) Profile() deploy.Profile {
	return deploy.Profile{Name: "fake-engine", DefaultImage: "gpuworks/fake-engine:test", HealthPath: "/health", Sleep: deploy.SleepCaps{Supported: true, Levels: []string{"1"}}}
}
func (p fakeEngineProfile) Build(params deploy.Params) deploy.ResolvedLaunch {
	env := map[string]string{}
	// Failure-injection knobs are copied only for the fake profile. This keeps
	// the deployment test on the real controller/agent/docker path without
	// adding test-only fields to production deployment DTOs.
	for _, key := range []string{"FAKE_ENGINE_CRASH_ON_START", "FAKE_ENGINE_OOM_EXIT", "FAKE_ENGINE_HEALTH_503", "FAKE_ENGINE_STARTUP_DELAY", "GPUWORKS_OBSERVATION_TEST_TOKEN"} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	argv := []string{"--model", params.ModelPath, "--port", strconv.Itoa(params.APIPort)}
	argv = append(argv, p.engineArgs...)
	// The fake-engine image already carries /usr/local/bin/fake-engine as its
	// Docker ENTRYPOINT. Returning an empty entrypoint here lets the runtime use
	// that image default instead of overriding it with a bare executable name.
	result := deploy.ResolvedLaunch{Argv: argv, Env: env}
	if p.explicitEntrypoint {
		result.Entrypoint = []string{"/usr/local/bin/fake-engine"}
	}
	return result
}

func TestDockerDriverDeploymentMountHealthProxyAndCleanup(t *testing.T) {
	const observationSecret = "l3-observation-secret"
	t.Setenv("GPUWORKS_OBSERVATION_TEST_TOKEN", observationSecret)
	env, api, depID, containerID, apiPort := startDockerDeployment(t, "gpuworks/fake-engine:test", "fake", deploy.CommandEngine, "community", `["/usr/local/bin/fake-engine","--dialect=sglang"]`, `["--model","/model","--port","{{PORT}}"]`, false)
	// Always remove the container if a later assertion aborts the test before
	// the normal deployment DELETE path.
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})
	key, err := env.Store().CreateAPIKey("l3", 0)
	if err != nil {
		t.Fatal(err)
	}
	chatReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"fake","messages":[{"role":"user","content":"hi"}]}`))
	chatReq.Header.Set("Authorization", "Bearer "+key)
	chatReq.Header.Set("Content-Type", "application/json")
	chatResp, err := http.DefaultClient.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	if chatResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(chatResp.Body)
		chatResp.Body.Close()
		t.Fatalf("chat proxy: %d %s", chatResp.StatusCode, b)
	}
	chatBody, _ := io.ReadAll(chatResp.Body)
	chatResp.Body.Close()
	if !strings.Contains(string(chatBody), "choices") {
		t.Fatalf("chat proxy body=%s", chatBody)
	}
	argv := containerArgv(t, containerID)
	if !containsAll(argv, "--model", "/model", "--port", strconv.Itoa(apiPort)) {
		t.Fatalf("assembled deploy argv=%v", argv)
	}
	assertDockerLaunchFacts(t, env, api, containerID, observationSecret)
	sleep, _ := api.PostJSON("/api/v1/deployments/"+strconv.FormatUint(uint64(depID), 10)+"/sleep", nil)
	if sleep.StatusCode != http.StatusOK {
		t.Fatalf("sleep status=%d", sleep.StatusCode)
	}
	sleep.Body.Close()
	env.Eventually(t, 20*time.Second, func() bool {
		current, _ := env.Store().ListInstances()
		return len(current) == 1 && current[0].SleepLevel == 1
	})
	sleepChatReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"fake","messages":[{"role":"user","content":"hi"}]}`))
	sleepChatReq.Header.Set("Authorization", "Bearer "+key)
	sleepChatReq.Header.Set("Content-Type", "application/json")
	sleepChat, err := http.DefaultClient.Do(sleepChatReq)
	if err != nil {
		t.Fatal(err)
	}
	sleepBody, _ := io.ReadAll(sleepChat.Body)
	sleepChat.Body.Close()
	if sleepChat.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(sleepBody), "asleep") {
		t.Fatalf("sleeping chat status=%d body=%s", sleepChat.StatusCode, sleepBody)
	}
	wake, _ := api.PostJSON("/api/v1/deployments/"+strconv.FormatUint(uint64(depID), 10)+"/wake", nil)
	if wake.StatusCode != http.StatusOK {
		t.Fatalf("wake status=%d", wake.StatusCode)
	}
	wake.Body.Close()
	env.Eventually(t, 20*time.Second, func() bool {
		current, _ := env.Store().ListInstances()
		return len(current) == 1 && current[0].SleepLevel == 0
	})
	wakeChatReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"fake","messages":[{"role":"user","content":"hi"}]}`))
	wakeChatReq.Header.Set("Authorization", "Bearer "+key)
	wakeChatReq.Header.Set("Content-Type", "application/json")
	wakeChat, err := http.DefaultClient.Do(wakeChatReq)
	if err != nil {
		t.Fatal(err)
	}
	wakeBody, _ := io.ReadAll(wakeChat.Body)
	wakeChat.Body.Close()
	if wakeChat.StatusCode != http.StatusOK || !strings.Contains(string(wakeBody), "choices") {
		t.Fatalf("woken chat status=%d body=%s", wakeChat.StatusCode, wakeBody)
	}
	deleted, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(depID), 10))
	if err != nil {
		t.Fatal(err)
	}
	deleteBody, _ := io.ReadAll(deleted.Body)
	deleted.Body.Close()
	if deleted.StatusCode != http.StatusOK && deleted.StatusCode != http.StatusAccepted && deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete deployment status=%d body=%s", deleted.StatusCode, deleteBody)
	}
	env.Eventually(t, 20*time.Second, func() bool {
		if _, err := env.Store().GetDeployment(depID); !errors.Is(err, store.ErrNotFound) {
			return false
		}
		current, err := env.Store().ListInstances()
		if err != nil || len(current) != 0 {
			return false
		}
		status, err := dockerdrv.New().Status(t.Context(), containerID)
		return err == nil && !status.Exists
	})
	t.Logf("deleted deployment %d, instances and container %s", depID, containerID)
}

type dockerInspectLaunchFacts struct {
	ID     string   `json:"Id"`
	Image  string   `json:"Image"`
	Path   string   `json:"Path"`
	Args   []string `json:"Args"`
	Config struct {
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		Env        []string `json:"Env"`
	} `json:"Config"`
}

func assertDockerLaunchFacts(t *testing.T, env *testenv.Env, api *testenv.APIClient, containerID, secret string) {
	t.Helper()
	instances, err := env.Store().ListInstances()
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	response, err := api.Get("/api/v1/instances/" + instances[0].UUID + "/launch-facts")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("launch facts status=%d body=%s", response.StatusCode, body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("launch facts leaked container secret: %s", body)
	}
	var facts struct {
		RuntimeObservationRelation dto.RuntimeObservationRelation   `json:"runtime_observation_relation"`
		RuntimeObservation         proto.InstanceRuntimeObservation `json:"runtime_observation"`
	}
	if err := json.Unmarshal(body, &facts); err != nil {
		t.Fatal(err)
	}
	if facts.RuntimeObservationRelation.Status != "current" {
		t.Fatalf("runtime observation relation=%+v", facts.RuntimeObservationRelation)
	}
	if facts.RuntimeObservation.Process.Status != proto.ObservationUnknown || facts.RuntimeObservation.Process.Reason != "not_collected" {
		t.Fatalf("process observation=%+v", facts.RuntimeObservation.Process)
	}

	var containerInspections []dockerInspectLaunchFacts
	inspectRaw, err := exec.Command("docker", "inspect", containerID).Output()
	if err != nil {
		t.Fatalf("docker inspect container: %v", err)
	}
	if err := json.Unmarshal(inspectRaw, &containerInspections); err != nil || len(containerInspections) != 1 {
		t.Fatalf("decode docker inspect: %v rows=%d", err, len(containerInspections))
	}
	actual := containerInspections[0]
	observed := facts.RuntimeObservation.Container
	if observed.Status != proto.ObservationObserved || observed.ContainerID != actual.ID || observed.ImageID != actual.Image {
		t.Fatalf("container identity observed=%+v inspect=%+v", observed, actual)
	}
	if !reflect.DeepEqual(observed.Entrypoint, nonNilL3Strings(actual.Config.Entrypoint)) || !reflect.DeepEqual(observed.Cmd, nonNilL3Strings(actual.Config.Cmd)) || observed.Path != actual.Path || !reflect.DeepEqual(observed.Args, nonNilL3Strings(actual.Args)) {
		t.Fatalf("container command observed=%+v inspect=%+v", observed, actual)
	}
	containerEnv := envSliceMap(actual.Config.Env)
	if containerEnv["GPUWORKS_OBSERVATION_TEST_TOKEN"] != secret || observed.Env["GPUWORKS_OBSERVATION_TEST_TOKEN"] != "<redacted>" {
		t.Fatalf("container env inspect=%q observed=%q", containerEnv["GPUWORKS_OBSERVATION_TEST_TOKEN"], observed.Env["GPUWORKS_OBSERVATION_TEST_TOKEN"])
	}

	var imageInspections []dockerInspectLaunchFacts
	imageRaw, err := exec.Command("docker", "image", "inspect", actual.Image).Output()
	if err != nil {
		t.Fatalf("docker image inspect immutable id: %v", err)
	}
	if err := json.Unmarshal(imageRaw, &imageInspections); err != nil || len(imageInspections) != 1 {
		t.Fatalf("decode docker image inspect: %v rows=%d", err, len(imageInspections))
	}
	imageObserved := facts.RuntimeObservation.Image
	imageActual := imageInspections[0]
	if imageObserved.Status != proto.ObservationObserved || imageObserved.ImageID != imageActual.ID || !reflect.DeepEqual(imageObserved.Entrypoint, nonNilL3Strings(imageActual.Config.Entrypoint)) || !reflect.DeepEqual(imageObserved.Cmd, nonNilL3Strings(imageActual.Config.Cmd)) {
		t.Fatalf("image observation=%+v inspect=%+v", imageObserved, imageActual)
	}
	if !reflect.DeepEqual(imageObserved.Env, envSliceMap(imageActual.Config.Env)) {
		t.Fatalf("image env observation=%+v inspect=%+v", imageObserved.Env, envSliceMap(imageActual.Config.Env))
	}
}

func nonNilL3Strings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func envSliceMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, item, found := strings.Cut(value, "=")
		if found {
			result[key] = item
		}
	}
	return result
}

// The deployment's /dev/shm size reaches the real container: docker reports it
// as HostConfig.ShmSize, and the preview-visible instance field agrees.
func TestDockerDriverDeploymentAppliesTheSharedMemorySize(t *testing.T) {
	const shm = int64(1 << 30)
	_, api, depID, containerID, _ := startDockerDeployment(t, "gpuworks/fake-engine:shm", "shm", deploy.CommandEngine, "community", "", "", true, map[string]any{"shm_bytes": shm})
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})
	out, err := exec.Command("docker", "inspect", "--format", "{{.HostConfig.ShmSize}}", containerID).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != strconv.FormatInt(shm, 10) {
		t.Fatalf("container ShmSize = %s, want %d", got, shm)
	}
	depResp, err := api.Get("/api/v1/deployments/" + strconv.FormatUint(uint64(depID), 10))
	if err != nil {
		t.Fatal(err)
	}
	var deployment struct {
		Instances []dto.Instance `json:"instances"`
	}
	if err := json.NewDecoder(depResp.Body).Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	depResp.Body.Close()
	if len(deployment.Instances) != 1 || deployment.Instances[0].ShmBytes != shm {
		t.Fatalf("instance shm = %+v, want %d", deployment.Instances, shm)
	}
	deleted, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(depID), 10))
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
}

func TestDockerDriverDeploymentImageEntryModeUsesImageEntrypoint(t *testing.T) {
	_, api, depID, containerID, apiPort := startDockerDeployment(t, "gpuworks/fake-engine:image-entry", "image-entry", deploy.CommandImageEntry, "community", `["/usr/local/bin/fake-engine","--dialect=sglang"]`, `["--model","/model","--port","{{PORT}}"]`, true)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})
	argv := containerArgv(t, containerID)
	if !containsAll(argv, "--dialect=sglang", "--engine-owned", "1", "--model", "/model", "--port", strconv.Itoa(apiPort)) {
		t.Fatalf("image-entry argv=%v", argv)
	}
	deleted, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(depID), 10))
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
}

func TestDockerDriverDeploymentManualModeSkipsEngineArgs(t *testing.T) {
	_, api, depID, containerID, apiPort := startDockerDeployment(t, "gpuworks/fake-engine:manual", "manual", deploy.CommandManual, "custom", `["/usr/local/bin/fake-engine","--dialect=sglang"]`, `["--model","/model","--port","{{PORT}}"]`, true)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerID).Run()
	})
	argv := containerArgv(t, containerID)
	if !containsAll(argv, "--dialect=sglang", "--model", "/model", "--port", strconv.Itoa(apiPort)) {
		t.Fatalf("manual argv=%v", argv)
	}
	if containsArg(argv, "--engine-owned") {
		t.Fatalf("manual mode unexpectedly kept engine args: %v", argv)
	}
	// The instance view reports the command that was actually shipped. The
	// fake engine records its argv without the executable (os.Args[1:]), so
	// compare the entrypoint executable separately from the rest, and never
	// resurrect the injected-args ledger.
	depResp, err := api.Get("/api/v1/deployments/" + strconv.FormatUint(uint64(depID), 10))
	if err != nil {
		t.Fatal(err)
	}
	var deployment struct {
		Instances []dto.Instance `json:"instances"`
	}
	if err := json.NewDecoder(depResp.Body).Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	depResp.Body.Close()
	if len(deployment.Instances) != 1 {
		t.Fatalf("instances=%d, want 1", len(deployment.Instances))
	}
	inst := deployment.Instances[0]
	command := append(append([]string{}, inst.Entrypoint...), inst.Argv...)
	if len(command) != len(argv)+1 || command[0] != "/usr/local/bin/fake-engine" {
		t.Fatalf("instance command %v does not lead with the container executable of argv %v", command, argv)
	}
	for index := range argv {
		if command[index+1] != argv[index] {
			t.Fatalf("instance command %v does not match container argv %v (token %d)", command, argv, index)
		}
	}
	if inst.CommandMode != string(deploy.CommandManual) {
		t.Fatalf("command_mode=%q, want manual", inst.CommandMode)
	}
	deleted, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(depID), 10))
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
}

func TestDockerDeploymentFailureInjectionReachesInstanceFailed(t *testing.T) {
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
	// The agent's exit detector samples container state on a 2s cadence, so a
	// 1s start budget buys exactly one sample: it can race container init
	// before the injected crash lands (then the health deadline misclassifies
	// the failure), and for oom it can inspect the exit before the log is
	// flushable. The exit cases get a budget the second sample still fits in;
	// what they assert is the classified message, not how fast the deadline
	// trips. The survival cases keep the 1s budget because their expected
	// failure IS the health deadline.
	for _, tc := range []struct {
		name, env, value, want string
		startTimeout           int
	}{
		{name: "crash", env: "FAKE_ENGINE_CRASH_ON_START", value: "1", want: "container exited", startTimeout: 10},
		{name: "oom", env: "FAKE_ENGINE_OOM_EXIT", value: "1", want: "container OOM-killed", startTimeout: 10},
		{name: "health-503", env: "FAKE_ENGINE_HEALTH_503", value: "1", want: "health check timed out", startTimeout: 1},
		{name: "startup-delay", env: "FAKE_ENGINE_STARTUP_DELAY", value: "3s", want: "health check timed out", startTimeout: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			env := testenv.New(t)
			env.SetControllerRetryPolicy(50*time.Millisecond, 1)
			env.SetControllerStartTimeout(tc.startTimeout)
			node := env.AddNode("docker-failure-"+tc.name, testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(dockerdrv.New()))
			env.WaitNodeOnline(t, node.Name)
			nodes, err := env.Store().ListNodes()
			if err != nil || len(nodes) != 1 {
				t.Fatalf("nodes=%+v err=%v", nodes, err)
			}
			modelDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fake-failure-" + tc.name, Revision: "main", Name: "fake-failure-" + tc.name, Arch: "qwen3", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: modelDir}); err != nil {
				t.Fatal(err)
			}
			api := env.API(t)
			body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": tc.name, "engine": "fake-engine", "image": "gpuworks/fake-engine:test"})
			resp, err := api.PostJSON("/api/v1/deployments", body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusCreated {
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("create deployment: %d %s", resp.StatusCode, data)
			}
			resp.Body.Close()
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				instances, _ := env.Store().ListInstances()
				for _, instance := range instances {
					if instance.State == proto.InstFailed {
						if !strings.Contains(instance.Message, tc.want) {
							t.Fatalf("failure message=%q, want %q", instance.Message, tc.want)
						}
						if instance.ContainerID != "" {
							t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", instance.ContainerID).Run() })
						}
						return
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatal("deployment did not reach failed state")
		})
	}
}

func TestDockerAgentRestartAdoptsExistingContainer(t *testing.T) {
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
	node := env.AddNode("docker-adoption-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(dockerdrv.New()))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fake-adoption", Revision: "main", Name: "fake-adoption", Arch: "qwen3", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: modelDir}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "fake-adoption", "engine": "fake-engine", "image": "gpuworks/fake-engine:test"})
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
	if dep.ID == 0 {
		t.Fatal("deployment id is zero")
	}
	env.Eventually(t, 45*time.Second, func() bool {
		instances, _ := env.Store().ListInstances()
		return len(instances) == 1 && instances[0].State == proto.InstRunning && instances[0].ContainerID != ""
	})
	instances, _ := env.Store().ListInstances()
	oldContainer := instances[0].ContainerID
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", oldContainer).Run() })

	env.RestartNode(t, node.Name)
	env.WaitNodeOnline(t, node.Name)
	env.Eventually(t, 30*time.Second, func() bool {
		current, _ := env.Store().ListInstances()
		return len(current) == 1 && current[0].State == proto.InstRunning && current[0].ContainerID == oldContainer
	})
	if out, err := exec.Command("docker", "inspect", oldContainer, "--format", "{{.State.Running}}").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("adopted container is not running: %v %s", err, out)
	}

	deleted, err := api.Delete("/api/v1/deployments/" + strconv.FormatUint(uint64(dep.ID), 10))
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
	env.Eventually(t, 30*time.Second, func() bool {
		current, _ := env.Store().ListInstances()
		if len(current) != 0 {
			return false
		}
		_, inspectErr := exec.Command("docker", "inspect", oldContainer).CombinedOutput()
		return inspectErr != nil
	})
}

func skipDocker(t *testing.T, err error) {
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("docker unavailable in strict mode: %v", err)
	}
	t.Skipf("docker unavailable: %v", err)
}

func ensureFakeImage(t *testing.T, root string) error {
	if err := exec.Command("docker", "image", "inspect", "gpuworks/fake-engine:test").Run(); err == nil {
		return nil
	}
	tarPath := filepath.Join(root, "test", "fake-engine", "fake-engine.tar")
	if _, err := os.Stat(tarPath); err != nil {
		return fmt.Errorf("fake-engine image not found and tar is absent: %s", tarPath)
	}
	if out, err := exec.Command("docker", "load", "-i", tarPath).CombinedOutput(); err != nil {
		return fmt.Errorf("docker load fake-engine: %v: %s", err, out)
	}
	return nil
}

func startDockerDeployment(t *testing.T, imageRef, servedName string, mode deploy.CommandMode, source string, entrypointDisplay string, cmdDisplay string, withEngineArgs bool, extraOverrides ...map[string]any) (*testenv.Env, *testenv.APIClient, uint, string, int) {
	t.Helper()
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
	registerFakeEngine(t, withEngineArgs)
	tagFakeEngineImage(t, "gpuworks/fake-engine:test", imageRef)
	env := testenv.New(t)
	apiPort := reservePort(t)
	env.SetControllerEnginePorts(apiPort, apiPort)
	nodeName := "docker-node-" + strings.ReplaceAll(strings.TrimPrefix(imageRef, "gpuworks/fake-engine:"), "/", "-")
	env.AddNode(nodeName, testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(dockerdrv.New()))
	env.WaitNodeOnline(t, nodeName)
	nodes, _ := env.Store().ListNodes()
	if len(nodes) != 1 {
		t.Fatalf("nodes=%+v", nodes)
	}
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: strings.ReplaceAll(imageRef, ":", "-"), Revision: "main", Name: strings.ReplaceAll(imageRef, ":", "-"), Arch: "qwen3", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: modelDir}); err != nil {
		t.Fatal(err)
	}
	cmdDisplay = strings.ReplaceAll(cmdDisplay, "{{PORT}}", strconv.Itoa(apiPort))
	if _, err := env.Store().UpsertImage(&store.Image{ImageRef: imageRef, Source: source, Engine: "fake-engine", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	// Deployments choose their command mode explicitly; the image catalog
	// carries no launch configuration (ADR 0031).
	overrides := map[string]any{"command_mode": string(mode)}
	for _, extra := range extraOverrides {
		for key, value := range extra {
			overrides[key] = value
		}
	}
	if mode != deploy.CommandEngine {
		overrides["entrypoint_display"] = entrypointDisplay
	}
	if mode == deploy.CommandManual {
		overrides["cmd_display"] = cmdDisplay
	}
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": servedName, "engine": "fake-engine", "image": imageRef, "sleepable": true, "container_overrides": overrides})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create deployment: %d %s", resp.StatusCode, data)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dep)
	resp.Body.Close()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		instances, _ := env.Store().ListInstances()
		if len(instances) == 1 {
			if instances[0].State == proto.InstRunning && instances[0].ContainerID != "" {
				return env, api, dep.ID, instances[0].ContainerID, apiPort
			}
			if instances[0].State == proto.InstFailed {
				t.Fatalf("deployment failed early: %+v", instances[0])
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	instances, _ := env.Store().ListInstances()
	t.Fatalf("deployment did not become running: %+v", instances)
	return nil, nil, 0, "", 0
}

func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.Port == 0 {
		t.Fatal("failed to reserve a TCP port")
	}
	return addr.Port
}

func registerFakeEngine(t *testing.T, extraArgs bool) {
	t.Helper()
	args := []string(nil)
	if extraArgs {
		args = []string{"--engine-owned", "1"}
	}
	deploy.RegisterEngine(fakeEngineProfile{engineArgs: args})
	t.Cleanup(func() {
		deploy.RegisterEngine(fakeEngineProfile{})
	})
}

func registerFakePreStartEngine(t *testing.T) {
	t.Helper()
	deploy.RegisterEngine(fakeEngineProfile{explicitEntrypoint: true})
	t.Cleanup(func() {
		deploy.RegisterEngine(fakeEngineProfile{})
	})
}

func tagFakeEngineImage(t *testing.T, src, dst string) {
	t.Helper()
	if out, err := exec.Command("docker", "tag", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("docker tag %s %s: %v %s", src, dst, err, out)
	}
}

func containerArgv(t *testing.T, containerID string) []string {
	t.Helper()
	argvPath := filepath.Join(t.TempDir(), "argv.json")
	if out, err := exec.Command("docker", "cp", containerID+":/tmp/fake-engine-argv.json", argvPath).CombinedOutput(); err != nil {
		t.Fatalf("copy assembled argv: %v %s", err, out)
	}
	argvRaw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read copied argv: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(argvRaw, &argv); err != nil {
		t.Fatalf("decode copied argv: %v", err)
	}
	return argv
}

func containsArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

func containsAll(argv []string, wants ...string) bool {
	for _, want := range wants {
		if !containsArg(argv, want) {
			return false
		}
	}
	return true
}
