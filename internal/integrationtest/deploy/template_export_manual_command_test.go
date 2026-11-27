package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

// A deployment that runs its own manual command, written the way the deploy
// form's editor writes one (set -eu, an export line per env key, then exec
// and the command), is saved as a template through the real API, the YAML is
// imported, the deployment is deleted, and a deployment made from the saved
// spec runs the same script: the exporter's replay check is what the create
// path does, not an assumption about it.
//
//	go test -count=1 -run '^TestTemplateExportOfAManualCommandRedeploysTheSameScriptThroughHTTP$' ./internal/integrationtest/deploy/
func TestTemplateExportOfAManualCommandRedeploysTheSameScriptThroughHTTP(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("template-manual", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	// A manual command reaches the runtime as one shell script, so the fake
	// engine cannot read its --served-model-name as an argv word; it answers
	// under the name the script serves, as the real engine would.
	node.Driver.SetEngineHandler(func(testenv.EngineInstance) http.Handler {
		return fakehandler.New(fakehandler.Options{Model: "template-manual"})
	})
	env.WaitNodeOnline(t, node.Name)
	storedNode, err := env.Store().NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	modelDir := testenv.ModelDir(t, "template-manual")
	if err := env.Store().SetStorageNodePath(env.Store().DefaultLocalStorageID(), storedNode.ID, filepath.Dir(modelDir)); err != nil {
		t.Fatal(err)
	}
	const commit = "48d788eca847d4d7548f375ad03d3c9312f6139e"
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/template-manual", Revision: commit, Name: "template-manual",
		Arch: "Qwen3ForCausalLM", Precision: "bf16", WeightBytes: 1 << 20,
		Meta:       `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
		Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: storedNode.ID, State: proto.MFPresent, Path: modelDir, Revision: commit, SizeBytes: model.WeightBytes}); err != nil {
		t.Fatal(err)
	}
	client := env.API(t)
	call := func(method, path string, body any, contentType string, want int) []byte {
		t.Helper()
		var reader io.Reader
		switch typed := body.(type) {
		case nil:
		case string:
			reader = strings.NewReader(typed)
		default:
			encoded, err := json.Marshal(typed)
			if err != nil {
				t.Fatal(err)
			}
			reader, contentType = strings.NewReader(string(encoded)), "application/json"
		}
		request, err := http.NewRequest(method, path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s (read error %v)", method, path, response.StatusCode, want, raw, err)
		}
		return raw
	}
	// running waits for the deployment's one instance to run and returns the
	// launch it was started with.
	running := func(deploymentPath string) proto.ResolvedLaunch {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			var detail struct {
				Instances []struct {
					UUID  string `json:"uuid"`
					State string `json:"state"`
				} `json:"instances"`
			}
			if err := json.Unmarshal(call(http.MethodGet, deploymentPath, nil, "", http.StatusOK), &detail); err != nil {
				t.Fatal(err)
			}
			if len(detail.Instances) == 1 && detail.Instances[0].State == "running" {
				var facts dto.InstanceLaunchFacts
				if err := json.Unmarshal(call(http.MethodGet, "/api/v1/instances/"+detail.Instances[0].UUID+"/launch-facts", nil, "", http.StatusOK), &facts); err != nil {
					t.Fatal(err)
				}
				if facts.Resolved.Value == nil {
					t.Fatalf("running instance has no resolved launch: %+v", facts)
				}
				return *facts.Resolved.Value
			}
			if time.Now().After(deadline) || (len(detail.Instances) > 0 && detail.Instances[0].State == "failed") {
				t.Fatalf("deployment did not become running: %s", call(http.MethodGet, deploymentPath, nil, "", http.StatusOK))
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	script := "set -eu\nexport VLLM_LOGGING_LEVEL='INFO'\n" +
		"exec vllm serve /model --served-model-name template-manual --port ${GPUWORKS_API_PORT} --tensor-parallel-size 1 --max-model-len 512"
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(call(http.MethodPost, "/api/v1/deployments", map[string]any{
		"name": "template-manual", "model_id": model.ID, "node_uuid": node.UUID,
		"engine": "vllm", "image": "example/template-manual:v1", "served_name": "template-manual",
		"container_overrides": map[string]any{"command_mode": "manual", "cmd_display": script},
	}, "", http.StatusCreated), &created); err != nil || created.ID == 0 {
		t.Fatalf("created=%+v: %v", created, err)
	}
	deploymentPath := fmt.Sprintf("/api/v1/deployments/%d", created.ID)
	original := running(deploymentPath)

	var exported struct {
		Saveable bool   `json:"saveable"`
		YAML     string `json:"yaml"`
		ModelID  string `json:"model_id"`
		SpecName string `json:"spec_name"`
	}
	if err := json.Unmarshal(call(http.MethodPost, deploymentPath+"/catalog-template", map[string]any{}, "", http.StatusOK), &exported); err != nil {
		t.Fatal(err)
	}
	if !exported.Saveable || !strings.Contains(exported.YAML, "manual_command:") || !strings.Contains(exported.YAML, "gpu.count != 1") {
		t.Fatalf("export of a manual command is not a saveable, pinned template: %+v", exported)
	}
	call(http.MethodPost, "/api/v1/inference-templates/import", exported.YAML, "application/yaml", http.StatusOK)
	segments := strings.Split(exported.ModelID, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	var saved struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(call(http.MethodGet, "/api/v1/inference-templates/model/"+strings.Join(segments, "/"), nil, "", http.StatusOK), &saved); err != nil || saved.ID == 0 {
		t.Fatalf("saved template=%+v: %v", saved, err)
	}

	// The public name is held by the first deployment until it is gone.
	call(http.MethodDelete, deploymentPath, nil, "", http.StatusAccepted)
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := client.Get(deploymentPath)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deleted deployment remains: status %d", response.StatusCode)
		}
		time.Sleep(25 * time.Millisecond)
	}

	var reused struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(call(http.MethodPost, "/api/v1/deployments", map[string]any{
		"name": "template-manual-again", "template_id": saved.ID, "template_spec_source": "user", "template_spec_name": exported.SpecName,
		"model_id": model.ID, "node_uuid": node.UUID, "served_name": "template-manual", "quick_params": map[string]any{},
	}, "", http.StatusCreated), &reused); err != nil || reused.ID == 0 {
		t.Fatalf("reused deployment=%+v: %v", reused, err)
	}
	again := running(fmt.Sprintf("/api/v1/deployments/%d", reused.ID))

	// The same script runs: the same executable and argv. The platform writes
	// each launch's own port into the script's --port, so ports are compared
	// as "the port this launch was given".
	assignedPort := regexp.MustCompile(`--port [0-9]+`)
	normalize := func(argv []string) []string {
		out := make([]string, len(argv))
		for i, word := range argv {
			out[i] = assignedPort.ReplaceAllString(word, "--port <assigned>")
		}
		return out
	}
	if !reflect.DeepEqual(original.Entrypoint, again.Entrypoint) || !reflect.DeepEqual(normalize(original.Argv), normalize(again.Argv)) || original.ModelMount != again.ModelMount {
		t.Fatalf("the saved template does not run the same command:\nbefore %q %q\nafter  %q %q", original.Entrypoint, original.Argv, again.Entrypoint, again.Argv)
	}
	if script := strings.Join(normalize(again.Argv), " "); !strings.Contains(script, "export VLLM_LOGGING_LEVEL='INFO'") ||
		!strings.Contains(script, "exec vllm serve /model --served-model-name 'template-manual' --port <assigned> --tensor-parallel-size 1 --max-model-len 512") {
		t.Fatalf("the redeployed launch is not the script: %q %q", again.Entrypoint, again.Argv)
	}
}
